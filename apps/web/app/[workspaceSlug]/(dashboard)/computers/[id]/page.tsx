"use client";

import { use } from "react";
import { ComputerDetailPage } from "@multica/views/computers";

export default function ComputerDetailRoute({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = use(params);
  return <ComputerDetailPage computerId={id} />;
}
