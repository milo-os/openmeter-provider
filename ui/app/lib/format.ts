export function relativeAge(timestamp: string | undefined): string {
  if (!timestamp) return "—";
  const diffMs = Date.now() - new Date(timestamp).getTime();
  const diffSecs = Math.floor(diffMs / 1000);
  if (diffSecs < 60) return `${diffSecs}s`;
  const diffMins = Math.floor(diffSecs / 60);
  if (diffMins < 60) return `${diffMins}m`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h`;
  const diffDays = Math.floor(diffHours / 24);
  return `${diffDays}d`;
}

export function phaseBadgeProps(phase: string): { type: string; theme: string } {
  switch (phase) {
    case "Ready":
      return { type: "success", theme: "light" };
    case "Provisioning":
      return { type: "info", theme: "light" };
    case "Failed":
      return { type: "danger", theme: "light" };
    default:
      return { type: "default", theme: "light" };
  }
}
