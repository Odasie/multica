import { useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ComputerDetailPage as SharedComputerDetailPage } from "@multica/views/computers";
import { useWorkspaceId } from "@multica/core/hooks";
import { computerDetailOptions } from "@multica/core/computers";
import { useDocumentTitle } from "@/hooks/use-document-title";

export function ComputerDetailPage() {
  const { id } = useParams<{ id: string }>();
  const wsId = useWorkspaceId();
  const { data: computer } = useQuery({
    ...computerDetailOptions(wsId, id ?? ""),
    enabled: !!wsId && !!id,
  });

  useDocumentTitle(computer?.name ?? "Computer");

  if (!id) return null;
  return <SharedComputerDetailPage computerId={id} />;
}
