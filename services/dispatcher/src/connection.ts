import { TunnelClient } from "./tunnel.js";

// Singleton tunnel client state
let tunnelClient: TunnelClient | null = null;
let connectionPromise: Promise<void> | null = null;

// Connection timeout in milliseconds
const CONNECTION_TIMEOUT_MS = 10000;

/**
 * Wraps a promise with a timeout.
 */
function withTimeout<T>(promise: Promise<T>, ms: number, message: string): Promise<T> {
  const timeout = new Promise<never>((_, reject) => {
    setTimeout(() => reject(new Error(message)), ms);
  });
  return Promise.race([promise, timeout]);
}

/**
 * Gets or creates the singleton tunnel client connection.
 * Reads BRIDGE_SERVER_ADDR from the environment to know where to connect.
 */
export async function getTunnelClient(): Promise<TunnelClient> {
  // Return existing client if connected
  if (tunnelClient && tunnelClient.connected) {
    return tunnelClient;
  }

  // Reset stale client
  if (tunnelClient && !tunnelClient.connected) {
    console.log("Tunnel client disconnected, reconnecting...");
    tunnelClient.disconnect();
    tunnelClient = null;
    connectionPromise = null;
  }

  // If already connecting, wait for that to complete
  if (connectionPromise) {
    await connectionPromise;
    return tunnelClient!;
  }

  const serverAddr = process.env.BRIDGE_SERVER_ADDR || "http://localhost:3000";

  // Create tunnel client
  tunnelClient = new TunnelClient({ serverAddr });

  // Connect with timeout
  connectionPromise = withTimeout(
    tunnelClient.connect(),
    CONNECTION_TIMEOUT_MS,
    `Connection to bridge server at ${serverAddr} timed out after ${CONNECTION_TIMEOUT_MS}ms`
  );

  try {
    await connectionPromise;
    console.log(`Connected to bridge server at ${serverAddr}`);
  } catch (error) {
    // Reset state on connection failure
    tunnelClient = null;
    connectionPromise = null;
    throw error;
  }

  return tunnelClient;
}

/**
 * Resets the tunnel client state. Call this on connection errors
 * to allow reconnection on the next request.
 */
export function resetTunnelClient(): void {
  tunnelClient = null;
  connectionPromise = null;
}

/**
 * Returns the current tunnel client if connected, or null if not.
 */
export function getCurrentTunnelClient(): TunnelClient | null {
  return tunnelClient;
}
