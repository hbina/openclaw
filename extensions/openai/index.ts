import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";
import { buildOpenAIProvider } from "./openai-provider.js";

export default definePluginEntry({
  id: "openai",
  name: "OpenAI Provider",
  description: "Bundled OpenAI-compatible Chat Completions provider",
  register(api) {
    api.registerProvider(buildOpenAIProvider());
  },
});
