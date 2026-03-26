type LogLevel = "debug" | "info" | "warn" | "error";

const LOG_LEVELS: Record<LogLevel, number> = {
  debug: 0,
  info: 1,
  warn: 2,
  error: 3,
};

function getConfiguredLevel(): number {
  const level = (process.env.LOG_LEVEL || "info").toLowerCase() as LogLevel;
  return LOG_LEVELS[level] ?? LOG_LEVELS.info;
}

const configuredLevel = getConfiguredLevel();

export const logger = {
  debug(message: string, ...args: unknown[]): void {
    if (configuredLevel <= LOG_LEVELS.debug) {
      console.log(`[DEBUG] ${message}`, ...args);
    }
  },
  info(message: string, ...args: unknown[]): void {
    if (configuredLevel <= LOG_LEVELS.info) {
      console.log(`[INFO] ${message}`, ...args);
    }
  },

  warn(message: string, ...args: unknown[]): void {
    if (configuredLevel <= LOG_LEVELS.warn) {
      console.warn(`[WARN] ${message}`, ...args);
    }
  },
  error(message: string, ...args: unknown[]): void {
    if (configuredLevel <= LOG_LEVELS.error) {
      console.error(`[ERROR] ${message}`, ...args);
    }
  },
};
