/**
 * Installs bundled plugin registration contract cases used across provider tests.
 */
import { describePluginRegistrationContract } from "./plugin-registration-contract.js";

type PluginRegistrationContractParams = Parameters<typeof describePluginRegistrationContract>[0];

export const pluginRegistrationContractCases = {
  anthropic: {
    pluginId: "anthropic",
    providerIds: ["anthropic"],
    mediaUnderstandingProviderIds: ["anthropic"],
    cliBackendIds: ["claude-cli"],
    requireDescribeImages: true,
  },
  openai: {
    pluginId: "openai",
    providerIds: ["openai"],
    speechProviderIds: ["openai"],
    realtimeTranscriptionProviderIds: ["openai"],
    realtimeVoiceProviderIds: ["openai"],
    mediaUnderstandingProviderIds: ["openai"],
    imageGenerationProviderIds: ["openai"],
    videoGenerationProviderIds: ["openai"],
    requireSpeechVoices: true,
    requireDescribeImages: true,
    requireGenerateImage: true,
    requireGenerateVideo: true,
  },
} satisfies Record<string, PluginRegistrationContractParams>;
