// Exposes local file URL helpers with fs-safe defaults.
import "./fs-safe-defaults.js";

// Local user-file URL helpers centralize encoded separator checks.
export {
  basenameFromMediaSource,
  hasEncodedFileUrlSeparator,
  safeFileURLToPath,
  trySafeFileURLToPath,
} from "@openclaw/fs-safe/advanced";
