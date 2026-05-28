package handler

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/cloudruntime"
)

// Cloud billing endpoints proxy to the same multica-cloud HTTP service
// that backs cloud-runtime (Fleet and Billing share `:8080` per the
// upstream README). All paths here forward verbatim to /api/v1/billing/*
// on the cloud side, mirroring the cloud-runtime handler shape:
//
//   - User-facing endpoints sit under /api/cloud-billing/* in our router
//     and require the regular Auth middleware. We inject the resolved
//     user_id as `X-User-ID` so the cloud side can scope owner queries.
//
//   - The Stripe webhook is the one outlier: it lives at
//     /api/webhooks/stripe (outside the Auth group), takes the raw
//     request body byte-for-byte, and forwards `Stripe-Signature` for
//     the cloud side to verify against its STRIPE_WEBHOOK_SECRET. The
//     upstream contract is explicit about this:
//     "webhook 使用原始请求体进行签名校验，不要在反向代理里改写 body."
//
// All proxy paths share `proxyCloudRuntime` for the standard JSON
// shape and only the webhook needs a custom raw-body forwarder.

// maxStripeWebhookBodySize bounds the raw body we'll forward upstream.
// Stripe's documented event payload upper bound is well under this;
// the cap exists to keep a malicious sender from making us read
// arbitrary memory before the upstream gets to reject the signature.
const maxStripeWebhookBodySize = 1 << 20 // 1 MiB

// stripeSignatureHeader is the canonical name of the header Stripe
// uses to ship its HMAC over the raw body. We forward whatever the
// client sent verbatim; the cloud side is the one that knows the
// shared secret and rejects on mismatch.
const stripeSignatureHeader = "Stripe-Signature"

// GetCloudBillingBalance forwards GET /api/v1/billing/balance.
//
// Returns the caller's wallet balance. Cloud reads `X-User-ID`; we
// stamp it from the authenticated context.
func (h *Handler) GetCloudBillingBalance(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodGet, "/api/v1/billing/balance", cloudRuntimeProxyOptions{
		withUserID: true,
	})
}

// ListCloudBillingTransactions forwards GET /api/v1/billing/transactions.
//
// The upstream supports `page` / `page_size`; we forward the query
// string unchanged.
func (h *Handler) ListCloudBillingTransactions(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodGet, "/api/v1/billing/transactions", cloudRuntimeProxyOptions{
		withUserID: true,
		withQuery:  true,
	})
}

// ListCloudBillingBatches forwards GET /api/v1/billing/batches.
//
// Returns paginated topup / bonus batches for the owner; same query
// shape as transactions.
func (h *Handler) ListCloudBillingBatches(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodGet, "/api/v1/billing/batches", cloudRuntimeProxyOptions{
		withUserID: true,
		withQuery:  true,
	})
}

// ListCloudBillingTopups forwards GET /api/v1/billing/topups.
func (h *Handler) ListCloudBillingTopups(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodGet, "/api/v1/billing/topups", cloudRuntimeProxyOptions{
		withUserID: true,
		withQuery:  true,
	})
}

// ListCloudBillingPriceTiers forwards GET /api/v1/billing/price-tiers.
//
// Per the upstream doc, this endpoint requires `X-User-ID` (it sits
// under the same auth fence as the rest of /api/v1/billing/*), even
// though the response is the same for every owner today. We stamp the
// header so cloud can audit who's listing tiers — and so the contract
// stays uniform if pricing later differentiates per-customer.
func (h *Handler) ListCloudBillingPriceTiers(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodGet, "/api/v1/billing/price-tiers", cloudRuntimeProxyOptions{
		withUserID: true,
	})
}

// CreateCloudBillingCheckoutSession forwards POST /api/v1/billing/checkout-sessions.
//
// Body shape (per upstream): `{tier_id, customer_email?}`. We don't
// care about its contents — proxyCloudRuntime validates only that
// it's syntactically JSON and forwards the bytes.
func (h *Handler) CreateCloudBillingCheckoutSession(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodPost, "/api/v1/billing/checkout-sessions", cloudRuntimeProxyOptions{
		withUserID: true,
		withBody:   true,
	})
}

// GetCloudBillingCheckoutSession forwards GET /api/v1/billing/checkout-sessions/{session_id}.
//
// The path param goes into the upstream URL; if missing we return 400
// before doing any network work.
func (h *Handler) GetCloudBillingCheckoutSession(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	// chi already URL-decodes the param, but it does NOT re-escape it
	// when we splice it into the upstream URL below. Stripe session
	// IDs are `cs_<base62>` so they're URL-safe by construction; if a
	// future event ever sends something containing `/` or `?`, that
	// would smuggle path/query segments. The cloud-runtime client
	// rejects paths that don't start with `/` but doesn't otherwise
	// validate; explicitly bail on suspicious characters here.
	for _, c := range sessionID {
		if c == '/' || c == '?' || c == '#' {
			writeError(w, http.StatusBadRequest, "invalid session_id")
			return
		}
	}
	h.proxyCloudRuntime(w, r, http.MethodGet, "/api/v1/billing/checkout-sessions/"+sessionID, cloudRuntimeProxyOptions{
		withUserID: true,
	})
}

// CreateCloudBillingPortalSession forwards POST /api/v1/billing/portal-sessions.
//
// Body shape is upstream-defined; can be empty. We treat it as
// optional JSON: cloud_runtime helper rejects empty bodies on
// withBody=true, so for portal-sessions we explicitly do NOT mark
// withBody and also send no body upstream. If the upstream contract
// later requires a body, switch this to withBody and let cloud
// validate.
func (h *Handler) CreateCloudBillingPortalSession(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodPost, "/api/v1/billing/portal-sessions", cloudRuntimeProxyOptions{
		withUserID: true,
	})
}

// HandleCloudBillingStripeWebhook is the public ingress for Stripe
// webhook deliveries. Three things are critical here and are *not*
// shared with the standard proxyCloudRuntime path:
//
//  1. NO authentication. Stripe POSTs from its own infrastructure;
//     we don't have a user context and don't try to invent one.
//     Application-layer auth is replaced by Stripe's HMAC signature,
//     which the upstream cloud service verifies. This route therefore
//     sits OUTSIDE the Auth group in router.go.
//
//  2. The body is forwarded byte-for-byte. Stripe's signature is
//     computed over the exact bytes it sent. We must NOT json.Unmarshal
//     /re-marshal, trim whitespace, or otherwise touch the payload —
//     even a single byte difference fails verification.
//
//  3. The `Stripe-Signature` header is forwarded verbatim. That's
//     the entire authentication channel from Stripe to the upstream.
//
// We DO bound the read with MaxBytesReader so a malicious or
// misconfigured sender can't make us buffer arbitrary memory before
// the upstream rejects on signature.
func (h *Handler) HandleCloudBillingStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if h.CloudRuntime == nil || !h.CloudRuntime.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "cloud runtime is not configured")
		return
	}

	// Body cap matches the JSON proxy. Stripe's documented payload
	// ceiling is much smaller; the limit is defense, not contract.
	r.Body = http.MaxBytesReader(w, r.Body, maxStripeWebhookBodySize)
	// io.ReadAll is appropriate here because the webhook body is
	// fully consumed before forwarding (Stripe signs the bytes; we
	// can't stream). Unlike the JSON proxy we deliberately do NOT
	// trim whitespace or json-validate — the upstream signature
	// check is computed over exactly what we received, so any
	// transformation here would silently break verification.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Pull whatever Stripe sent — we forward all values rather than
	// the first only, in case Stripe ever changes from the v=… split
	// across multiple header lines (they currently use a single line).
	headers := http.Header{}
	if sigs := r.Header.Values(stripeSignatureHeader); len(sigs) > 0 {
		headers[stripeSignatureHeader] = sigs
	}
	// Forward the original Content-Type if set, since the cloud side
	// hands the raw body to a Stripe SDK that doesn't strictly require
	// JSON content-type but doesn't object to it. The cloudruntime
	// client defaults to `application/json` when a body is present;
	// for a webhook that's the same value Stripe sends, so we don't
	// override it explicitly.

	resp, err := h.CloudRuntime.Do(r.Context(), cloudruntime.Request{
		Method:    http.MethodPost,
		Path:      "/api/v1/webhooks/stripe",
		Body:      body,
		Headers:   headers,
		RequestID: cloudRuntimeRequestID(r),
		// Intentionally no UserID — webhook is unauthenticated by
		// design; injecting an empty header would still be harmless,
		// but staying explicit makes the contract obvious to readers.
	})
	if err != nil {
		writeCloudRuntimeError(w, r, err)
		return
	}
	writeCloudRuntimeResponse(w, resp)
}
