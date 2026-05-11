# Chat 附件 & 图片支持 — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 让 chat 输入框支持 paste 图片、拖拽文件、点按钮上传,跟 issue 评论一样,并把 attachment 关联到 chat_session / chat_message。

**Architecture:** 完全沿用现有 comment 的"上传时关联到上一层资源(session ≈ issue)、send 时把 message_id 回填(message ≈ comment)"的两阶段模式。Tiptap 的 file-upload extension、`useFileUpload` hook、`FileUploadButton`、`useFileDropZone`、`FileDropOverlay` 都已就绪——只是 chat-input 当初迁到 ContentEditor 时没接线(commit `6097f739`)。后端 `attachment` 表加两列 `chat_session_id` / `chat_message_id`,handler 加一条 `LinkAttachmentsToChatMessage` 链路。前端关键变化:**上传发起时若还没 chat_session,先懒创建一个**,这样 attachment 行总有 session_id 兜底(对称于 comment 上传时 issue_id 一定存在)。

**Tech Stack:** Go (chi, sqlc, pgx) + Postgres / Next.js + Electron + Tiptap + TanStack Query + Zustand。

**Reference 同构实现(每个 Task 都会回到这里对照):**
- `packages/views/issues/components/comment-input.tsx:1-112` — 前端模板
- `server/internal/handler/comment.go:126-237` — 后端模板(`CreateCommentRequest` + `linkAttachmentsByIDs`)
- `server/pkg/db/queries/attachment.sql` — `LinkAttachmentsToComment` 模板
- `server/migrations/029_attachment.up.sql` — schema 模板

---

## Open Decision(执行前先和用户确认一次)

**懒创建 session 的标题用什么?** 现在 `createSession` 用消息内容前 50 字做标题。如果用户先拖图后输入文字,session 在上传文件时就建好了,标题此时为空。两个选项:

1. **占位标题 "New chat" / "未命名会话"**,首条 user 消息发出后用消息内容 `UPDATE` 标题。**推荐(简单、对会话列表友好)**。
2. **延后到 send**:上传时让 attachment 先只挂 workspace_id,send(同时创建 session) 后一并回填 session_id + message_id。更对称但要多一条 `LinkAttachmentsToChatSessionAndMessage` query 和一段悬空清理逻辑。

执行 Task 1 之前回 issue 问一次,默认走方案 1。

---

## Task 0:开 worktree(如已在 worktree 内可跳过)

执行所在工作目录已经是 `agent/n-y/00006373` 分支;直接在此工作。

---

## Task 1:DB migration — 给 attachment 加 chat 列

**Files:**
- Create: `server/migrations/0NN_attachment_chat_columns.up.sql`(NN = 当前最大 migration 号 + 1,跑 `ls server/migrations/ | tail -5` 确认)
- Create: `server/migrations/0NN_attachment_chat_columns.down.sql`

**Step 1: 确认下一个 migration 序号**

```bash
ls server/migrations/ | grep -E '^[0-9]+_' | sort -n | tail -3
```
Expected:看到最新的 0NN_xxx 文件,取 NN+1 作为新号。

**Step 2: 写 up migration**

```sql
-- 0NN_attachment_chat_columns.up.sql
ALTER TABLE attachment
  ADD COLUMN chat_session_id UUID REFERENCES chat_session(id) ON DELETE CASCADE,
  ADD COLUMN chat_message_id UUID REFERENCES chat_message(id) ON DELETE CASCADE;

CREATE INDEX idx_attachment_chat_session
  ON attachment(chat_session_id)
  WHERE chat_session_id IS NOT NULL;

CREATE INDEX idx_attachment_chat_message
  ON attachment(chat_message_id)
  WHERE chat_message_id IS NOT NULL;
```

**Step 3: 写 down migration**

```sql
-- 0NN_attachment_chat_columns.down.sql
DROP INDEX IF EXISTS idx_attachment_chat_message;
DROP INDEX IF EXISTS idx_attachment_chat_session;
ALTER TABLE attachment
  DROP COLUMN chat_message_id,
  DROP COLUMN chat_session_id;
```

**Step 4: 跑 migration**

```bash
make migrate-up
```
Expected:打印新 migration 已应用,exit 0。

**Step 5: 回归确认**

```bash
psql "$DATABASE_URL" -c "\d attachment" | grep chat_
```
Expected:看到 `chat_session_id`、`chat_message_id` 两列。

**Step 6: Commit**

```bash
git add server/migrations/0NN_attachment_chat_columns.up.sql server/migrations/0NN_attachment_chat_columns.down.sql
git commit -m "feat(db): add chat_session_id/chat_message_id to attachment"
```

---

## Task 2:sqlc queries — 扩展 CreateAttachment + 新增 LinkAttachmentsToChatMessage

**Files:**
- Modify: `server/pkg/db/queries/attachment.sql`
- Auto-generated: `server/pkg/db/attachment.sql.go`(由 sqlc 重新生成)

**Step 1: 改 CreateAttachment 接受 chat_session_id**

把 `attachment.sql` 顶部的 `CreateAttachment` 改成:

```sql
-- name: CreateAttachment :one
INSERT INTO attachment (
  id, workspace_id, issue_id, comment_id, chat_session_id,
  uploader_type, uploader_id, filename, url, content_type, size_bytes
)
VALUES (
  $1, $2, sqlc.narg(issue_id), sqlc.narg(comment_id), sqlc.narg(chat_session_id),
  $3, $4, $5, $6, $7, $8
)
RETURNING *;
```

**Step 2: 新增 LinkAttachmentsToChatMessage**

在 `LinkAttachmentsToComment` 下面加:

```sql
-- name: LinkAttachmentsToChatMessage :exec
UPDATE attachment
SET chat_message_id = $1
WHERE chat_session_id = $2
  AND chat_message_id IS NULL
  AND id = ANY($3::uuid[]);

-- name: ListAttachmentsByChatMessage :many
SELECT * FROM attachment
WHERE chat_message_id = $1 AND workspace_id = $2
ORDER BY created_at ASC;
```

**Step 3: 重新生成 sqlc**

```bash
make sqlc
```
Expected:`server/pkg/db/attachment.sql.go` 有改动,新增 `LinkAttachmentsToChatMessage`、`ListAttachmentsByChatMessage` 方法,`CreateAttachmentParams` 多了 `ChatSessionID pgtype.UUID`。

**Step 4: 编译验证**

```bash
cd server && go build ./...
```
Expected:exit 0(此刻 handler 还没用新字段,但生成的代码自身能编)。

**Step 5: Commit**

```bash
git add server/pkg/db/queries/attachment.sql server/pkg/db/
git commit -m "feat(db): sqlc — chat_session_id on CreateAttachment + LinkAttachmentsToChatMessage"
```

---

## Task 3:Go 单元测试 — UploadFile 接受 chat_session_id

**Files:**
- Modify: `server/internal/handler/file_test.go`(如不存在则 Create)

**Step 1: 写 failing test**

新增 `TestUploadFile_AttachesToChatSession`:在 workspace 下建 user、agent、chat_session,然后 multipart POST `/api/upload-file`,form 字段 `chat_session_id=<session uuid>` + `file=<small png>`,断言返回 200、attachment 行在 DB 里 `chat_session_id` 已填、`comment_id`/`issue_id` 为 NULL、URL 非空。

参考 `comment_test.go` / `file_test.go` 现有 fixture helpers(`testCreateWorkspace` 等)。

**Step 2: 跑测试验证它失败**

```bash
cd server && go test ./internal/handler/ -run TestUploadFile_AttachesToChatSession -v
```
Expected:FAIL("chat_session_id" form 字段未被识别)。

**Step 3: 在 UploadFile handler 里实现 chat_session_id 分支**

修改 `server/internal/handler/file.go:104-242` —— 在 `comment_id` 解析后插入:

```go
if chatSessionID := r.FormValue("chat_session_id"); chatSessionID != "" {
    chatSessionUUID, ok := parseUUIDOrBadRequest(w, chatSessionID, "chat_session_id")
    if !ok {
        return
    }
    session, err := h.Queries.GetChatSession(r.Context(), chatSessionUUID)
    if err != nil || uuidToString(session.WorkspaceID) != workspaceID {
        writeError(w, http.StatusForbidden, "invalid chat_session_id")
        return
    }
    // Re-use the existing private-agent gate so the user can still reach this session.
    if _, ok := h.gateChatSessionForUser(w, r, userID, workspaceID, chatSessionID); !ok {
        return
    }
    params.ChatSessionID = session.ID
}
```

注意:`gateChatSessionForUser` 当前签名会写 401/403 并返回 ok=false,直接复用即可——不要重复写错误响应。如果它不存在或签名不合,看 `chat.go:340-356`。

**Step 4: 跑测试验证通过**

```bash
cd server && go test ./internal/handler/ -run TestUploadFile_AttachesToChatSession -v
```
Expected:PASS。

**Step 5: Commit**

```bash
git add server/internal/handler/file.go server/internal/handler/file_test.go
git commit -m "feat(file): upload-file accepts chat_session_id form field"
```

---

## Task 4:Go 单元测试 — SendChatMessage 回填 attachment_ids

**Files:**
- Modify: `server/internal/handler/chat_test.go`(如不存在则 Create)
- Modify: `server/internal/handler/chat.go`

**Step 1: 写 failing test**

`TestSendChatMessage_LinksAttachments`:建 workspace+user+agent+session,先调 UploadFile 拿到 attachment id A(挂在 session 上,message_id 为空);再 POST `/api/chat-sessions/{id}/messages` body `{"content":"hi ![](url)","attachment_ids":["A"]}`;断言响应 201、DB 里 attachment A 的 chat_message_id 等于响应里的 message_id。

**Step 2: 跑测试验证它失败**

```bash
cd server && go test ./internal/handler/ -run TestSendChatMessage_LinksAttachments -v
```
Expected:FAIL(请求体多余字段被忽略,attachment.chat_message_id 仍为 NULL)。

**Step 3: 改 SendChatMessageRequest + handler**

在 `server/internal/handler/chat.go:307-309`:

```go
type SendChatMessageRequest struct {
    Content       string   `json:"content"`
    AttachmentIDs []string `json:"attachment_ids"`
}
```

在 `chat.go` 中 `CreateChatMessage` 之后(`server/internal/handler/chat.go:367` 附近):

```go
attachmentIDs, ok := parseUUIDSliceOrBadRequest(w, req.AttachmentIDs, "attachment_ids")
if !ok {
    return
}
if len(attachmentIDs) > 0 {
    if err := h.Queries.LinkAttachmentsToChatMessage(r.Context(), db.LinkAttachmentsToChatMessageParams{
        ChatMessageID: msg.ID,
        ChatSessionID: session.ID,
        Column3:       attachmentIDs, // sqlc 默认列名,跑 make sqlc 看实际字段名
    }); err != nil {
        slog.Warn("link chat attachments failed", "error", err, "message_id", uuidToString(msg.ID))
        // 不阻断 send — attachment 仅是富文本附件,主要内容已经发出去。
    }
}
```

注意 parseUUIDSliceOrBadRequest 解析放在 decode 之后、`CreateChatMessage` 之前,这样无效输入早期返回 400。

**Step 4: 跑测试验证通过**

```bash
cd server && go test ./internal/handler/ -run TestSendChatMessage_LinksAttachments -v
```
Expected:PASS。

**Step 5: 回归跑全部 chat / file handler 测试**

```bash
cd server && go test ./internal/handler/ -run 'TestSendChatMessage|TestUploadFile' -v
```
Expected:全 PASS。

**Step 6: Commit**

```bash
git add server/internal/handler/chat.go server/internal/handler/chat_test.go
git commit -m "feat(chat): SendChatMessage links uploaded attachments to the new message"
```

---

## Task 5:前端 api client — uploadFile + sendChatMessage 携带新字段

**Files:**
- Modify: `packages/core/api/client.ts:1024-1077`
- Modify: `packages/core/api/client.test.ts`(若有)

**Step 1: 写 failing test**

在 `client.test.ts` 加用例:`uploadFile(file, { chatSessionId: "..." })` 发的 FormData 包含 `chat_session_id`;`sendChatMessage(sid, content, ["att1","att2"])` 的 body 是 `{content, attachment_ids: [...]}`。

可以用 `vi.fn()` 替 fetch + 断言入参。

**Step 2: 跑测试验证它失败**

```bash
pnpm --filter @multica/core exec vitest run api/client.test.ts
```
Expected:FAIL。

**Step 3: 改 client.ts**

```ts
// line 1024
async uploadFile(
  file: File,
  opts?: { issueId?: string; commentId?: string; chatSessionId?: string },
): Promise<Attachment> {
  const formData = new FormData();
  formData.append("file", file);
  if (opts?.issueId) formData.append("issue_id", opts.issueId);
  if (opts?.commentId) formData.append("comment_id", opts.commentId);
  if (opts?.chatSessionId) formData.append("chat_session_id", opts.chatSessionId);
  // ... rest unchanged
}

// line 1077
async sendChatMessage(
  sessionId: string,
  content: string,
  attachmentIds?: string[],
): Promise<SendChatMessageResponse> {
  // ... existing fetch, body becomes JSON.stringify({ content, attachment_ids: attachmentIds })
}
```

记得 response 仍走现有 zod schema(`parseWithFallback`),无需新 schema。

**Step 4: 跑测试验证通过**

```bash
pnpm --filter @multica/core exec vitest run api/client.test.ts
```
Expected:PASS。

**Step 5: Commit**

```bash
git add packages/core/api/client.ts packages/core/api/client.test.ts
git commit -m "feat(api): uploadFile accepts chatSessionId; sendChatMessage accepts attachmentIds"
```

---

## Task 6:扩展 useFileUpload 的 UploadContext

**Files:**
- Modify: `packages/core/hooks/use-file-upload.ts:11-15`(UploadContext)
- 该 hook 没有独立测试时,跳过 TDD;若有则按 TDD。

**Step 1:**

```ts
export interface UploadContext {
  issueId?: string;
  commentId?: string;
  chatSessionId?: string;
}
```

在 `upload` 内部传给 `api.uploadFile`:

```ts
const att: Attachment = await api.uploadFile(file, {
  issueId: ctx?.issueId,
  commentId: ctx?.commentId,
  chatSessionId: ctx?.chatSessionId,
});
```

**Step 2: typecheck**

```bash
pnpm typecheck
```
Expected:exit 0。

**Step 3: Commit**

```bash
git add packages/core/hooks/use-file-upload.ts
git commit -m "feat(core): useFileUpload supports chatSessionId context"
```

---

## Task 7:chat-window 提供 onUploadFile(含 ensureSession 懒创建)

**Files:**
- Modify: `packages/views/chat/components/chat-window.tsx:190-270`
- Modify: `packages/views/chat/components/chat-input.tsx`(加 prop,下一个 Task 实现)

**Step 1: 抽 ensureSession**

把现有 `handleSend` 内部的"如果没 sessionId 就 createSession.mutateAsync"逻辑提取为独立函数:

```ts
const ensureSession = useCallback(async (titleSeed: string): Promise<string | null> => {
  if (activeSessionId) return activeSessionId;
  if (!activeAgent) return null;
  const session = await createSession.mutateAsync({
    agent_id: activeAgent.id,
    title: titleSeed.slice(0, 50) || "New chat", // 若决策选方案 1
  });
  setActiveSession(session.id);
  return session.id;
}, [activeSessionId, activeAgent, createSession, setActiveSession]);
```

`handleSend` 改成调 `ensureSession(finalContent)`。

**Step 2: handleUploadFile**

```ts
const { uploadWithToast } = useFileUpload(api);

const handleUploadFile = useCallback(async (file: File) => {
  const sessionId = await ensureSession("");
  if (!sessionId) return null;
  return uploadWithToast(file, { chatSessionId: sessionId });
}, [ensureSession, uploadWithToast]);
```

**Step 3: handleSend 接收 attachment ids**

把 onSend 签名升级:`(content: string, attachmentIds?: string[]) => Promise<void>`。
`handleSend` 内部 `await api.sendChatMessage(sessionId, finalContent, attachmentIds)`。

**Step 4: 透传 prop**

```tsx
<ChatInput
  onSend={handleSend}
  onUploadFile={handleUploadFile}
  // ...
/>
```

**Step 5: typecheck**

```bash
pnpm typecheck
```
Expected:exit 0(ChatInput prop 还没加,所以预期 ChatInputProps 一行 TS 错误;先继续到 Task 8,合并 commit)。

**Step 6: 暂不 commit,等 Task 8 一并提交**

---

## Task 8:chat-input UI 接线 — useFileDropZone + FileUploadButton + onUploadFile

**Files:**
- Modify: `packages/views/chat/components/chat-input.tsx`

**Step 1: 写 failing test**

`packages/views/chat/components/chat-input.test.tsx`(若已存在追加用例,否则 Create):
渲染 ChatInput,模拟拖一个 File 进容器,断言 `onUploadFile` mock 被调用;模拟点击 FileUploadButton 选文件,断言 `onUploadFile` 被调用;模拟 send,断言传给 onSend 的第二个参数包含上传后拿到的 attachment id。

Mock `useFileUpload` / `api` 用 `vi.hoisted`。

**Step 2: 跑测试验证它失败**

```bash
pnpm --filter @multica/views exec vitest run chat/components/chat-input.test.tsx
```
Expected:FAIL。

**Step 3: 实现**

参考 `packages/views/issues/components/comment-input.tsx:1-110` 移植以下到 chat-input:

```tsx
import { ContentEditor, type ContentEditorRef, useFileDropZone, FileDropOverlay } from "../../editor";
import { FileUploadButton } from "@multica/ui/components/common/file-upload-button";

interface ChatInputProps {
  onSend: (content: string, attachmentIds?: string[]) => void;
  onUploadFile?: (file: File) => Promise<{ id: string; link: string } | null>;
  // ...existing props
}

// inside component:
const uploadMapRef = useRef<Map<string, string>>(new Map());
const { isDragOver, dropZoneProps } = useFileDropZone({
  onDrop: (files) => files.forEach((f) => editorRef.current?.uploadFile(f)),
});
const handleUpload = useCallback(async (file: File) => {
  const result = await onUploadFile?.(file);
  if (result) uploadMapRef.current.set(result.link, result.id);
  return result ?? null;
}, [onUploadFile]);

// handleSend:
const content = ...; // existing
const activeIds: string[] = [];
for (const [url, id] of uploadMapRef.current) {
  if (content.includes(url)) activeIds.push(id);
}
onSend(content, activeIds.length > 0 ? activeIds : undefined);
uploadMapRef.current.clear();
// ...existing clearContent / blur / clearInputDraft

// JSX 外层 div 加 {...dropZoneProps};ContentEditor 加 onUploadFile={handleUpload};
// 右下 SubmitButton 旁加 FileUploadButton size="sm" onSelect={(f) => editorRef.current?.uploadFile(f)};
// {isDragOver && <FileDropOverlay />} 加进容器
```

注意 chat 的容器层级有 `noAgent` 时的 pointer-events-none / opacity-60 wrapper,FileDropOverlay 应放在内层 rounded 容器里,避免 disabled 状态下还能响应拖拽。

**Step 4: 跑测试验证通过**

```bash
pnpm --filter @multica/views exec vitest run chat/components/chat-input.test.tsx
```
Expected:PASS。

**Step 5: 跑 typecheck + lint + 全部前端 vitest**

```bash
pnpm typecheck && pnpm lint && pnpm test
```
Expected:全部 exit 0。

**Step 6: Commit(Task 7 + 8 合并)**

```bash
git add packages/views/chat/components/chat-input.tsx \
        packages/views/chat/components/chat-input.test.tsx \
        packages/views/chat/components/chat-window.tsx
git commit -m "feat(chat): support paste/drag/upload attachments in chat input"
```

---

## Task 9:手测两端 dev 服务

**Step 1: 启动**

```bash
make dev
```

**Step 2: 三条用例**

在 web (`http://localhost:3000/<workspace>`) 打开 chat:
1. 在输入框 paste 一张 PNG → 期待:出现上传中卡片,完成后变成图片预览;send 后图片显示在消息气泡里。
2. 拖一个 PDF 到输入框 → 期待:drop overlay 显示,松手开始上传,完成后出现文件卡片;send 后消息含 PDF 链接。
3. 点 FileUploadButton 选文件 → 同 2。

desktop:`pnpm dev:desktop` 重复以上三条。

**Step 3: DB 检查**

```bash
psql "$DATABASE_URL" -c \
  "SELECT id, chat_session_id, chat_message_id, filename FROM attachment WHERE chat_session_id IS NOT NULL ORDER BY created_at DESC LIMIT 5;"
```
Expected:看到刚才上传的 attachment 行,`chat_message_id` 在 send 后已填。

**Step 4: 没问题就继续**

---

## Task 10:E2E 测试

**Files:**
- Create: `e2e/tests/chat-attachments.spec.ts`

**Step 1: 写 E2E**

参考 `e2e/tests/` 现有的 chat / comment 测试结构。最少一条:登录 → 进 chat → 上传图片(via setInputFiles)→ 等到附件卡片 ready → send → 等服务器响应 → 断言消息列表里有图片 URL + DB 检查 attachment 行有 chat_message_id。

**Step 2: 跑 E2E**

```bash
pnpm exec playwright test e2e/tests/chat-attachments.spec.ts
```
Expected:PASS。

**Step 3: Commit**

```bash
git add e2e/tests/chat-attachments.spec.ts
git commit -m "test(e2e): chat input attachment upload + send round-trip"
```

---

## Task 11:i18n 字符串(若有 chat 输入相关新文案)

**Files:**
- Modify: `packages/views/locales/zh-Hans/chat.json`
- Modify: `packages/views/locales/en/chat.json`

如果 FileUploadButton 自带 tooltip / drop overlay 提示属于 chat namespace 的新键,按 `apps/docs/content/docs/developers/conventions.zh.mdx` 的命名约定补 zh-Hans + en 两套。若复用 `common`/`editor` 现有键,跳过此 Task。

**Step 1: 看是不是真的需要新 key**

```bash
grep -rn "drop_files\|upload_file" packages/views/locales/ packages/views/editor/ | head
```

**Step 2: 按需补 + commit**

```bash
git add packages/views/locales/
git commit -m "chore(i18n): chat attachment strings"
```

---

## Task 12:全量 check + PR

**Step 1: 跑全量验证**

```bash
make check
```
Expected:typecheck / lint / vitest / go test / playwright 全部通过。

**Step 2: 自查清单**

- [ ] migration 上下行配套
- [ ] `make sqlc` 已跑过、生成代码 commit 在内
- [ ] 后端两条新测试:UploadFile-chat / SendChatMessage-attachments
- [ ] 前端 chat-input 单测覆盖 paste / drop / button / send 携带 ids
- [ ] E2E 至少一条端到端通过
- [ ] desktop 和 web 都手测过
- [ ] 没有遗漏的 i18n 字符串硬编码
- [ ] 跨 workspace / 跨 session 的 attachment 不会串(`gateChatSessionForUser` 已覆盖)

**Step 3: 开 PR**

```bash
gh pr create --title "feat(chat): support attachments & images" --body "$(cat <<'EOF'
## Summary
- 复用 comment 的两阶段 attachment 流程,上传时挂 chat_session,send 后回填 chat_message
- 前端 chat-input 接线 useFileUpload / useFileDropZone / FileUploadButton(commit 6097f739 迁移时刻意没接线)
- attachment 表新增 chat_session_id / chat_message_id 两列 + 索引

## Test plan
- [ ] make check 全绿
- [ ] web + desktop 都跑通 paste / drag / button 三种上传方式
- [ ] E2E 端到端通过
EOF
)"
```

---

## Out of scope(本 plan 不做,留作 follow-up)

- Agent 端是否要在 prompt 里显式列 attachment 元数据?当前 daemon 通过 markdown URL 已能拿到,vision 模型直接消费;如果需要更结构化的 attachment metadata 上下文,另开 issue。
- 旧 chat_session 标题为 "New chat" 的回填(若选 Open Decision 方案 1)——加个简单 trigger 或在 send 时若 title==`'New chat'` 就用首条消息内容 patch,可放在本 plan 的 Task 7 顺手做或 follow-up。
- Attachment 大小 / mime 类型在 chat 上下文的限制(目前共享 `MAX_FILE_SIZE` 100MB 上限),若产品上 chat 要更严格,另开。
