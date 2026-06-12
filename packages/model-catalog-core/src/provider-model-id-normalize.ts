export function normalizeTogetherModelId(id: string): string {
  if (id === "moonshotai/Kimi-K2.5") {
    return "moonshotai/Kimi-K2.6";
  }
  return id;
}
