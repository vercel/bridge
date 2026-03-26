import { getTunnelClient, resetTunnelClient, getCurrentTunnelClient } from "./connection.js";

export interface RequestInfo {
  method: string;
  url: string;
  headers?: Record<string, string>;
  body?: string;
  remoteIp?: string;
  remotePort?: number;
  localIp?: string;
  localPort?: number;
}

export interface ResponseWriter {
  status(code: number): ResponseWriter;
  setHeader(name: string, value: string): void;
  send(body: string): void;
  json(body: object): void;
}

/**
 * Builds a raw HTTP request buffer from request info.
 */
function buildHttpRequest(req: RequestInfo): Uint8Array {
  const lines: string[] = [];
  lines.push(`${req.method} ${req.url} HTTP/1.1`);

  if (req.headers) {
    for (const [key, value] of Object.entries(req.headers)) {
      lines.push(`${key}: ${value}`);
    }
  }

  if (req.body) {
    if (!req.headers?.["content-length"] && !req.headers?.["Content-Length"]) {
      lines.push(`Content-Length: ${Buffer.byteLength(req.body)}`);
    }
  }

  lines.push(""); // blank line separating headers from body
  let raw = lines.join("\r\n") + "\r\n";
  if (req.body) {
    raw += req.body;
  }

  return new TextEncoder().encode(raw);
}

/**
 * Parses a raw HTTP response buffer into status, headers, and body.
 */
function parseHttpResponse(data: Uint8Array): {
  statusCode: number;
  statusText: string;
  headers: Record<string, string>;
  body: Uint8Array;
} {
  const text = new TextDecoder().decode(data);
  const headerEnd = text.indexOf("\r\n\r\n");
  if (headerEnd === -1) {
    return { statusCode: 502, statusText: "Bad Gateway", headers: {}, body: new Uint8Array() };
  }

  const headerSection = text.slice(0, headerEnd);
  const bodyStart = headerEnd + 4; // skip \r\n\r\n
  const bodyBytes = data.slice(new TextEncoder().encode(text.slice(0, bodyStart)).length);

  const headerLines = headerSection.split("\r\n");
  const statusLine = headerLines[0];
  const statusMatch = statusLine.match(/^HTTP\/\d\.\d\s+(\d+)\s*(.*)/);
  const statusCode = statusMatch ? parseInt(statusMatch[1], 10) : 502;
  const statusText = statusMatch ? statusMatch[2] : "Bad Gateway";

  const headers: Record<string, string> = {};
  for (let i = 1; i < headerLines.length; i++) {
    const colonIdx = headerLines[i].indexOf(":");
    if (colonIdx !== -1) {
      const key = headerLines[i].slice(0, colonIdx).trim();
      const value = headerLines[i].slice(colonIdx + 1).trim();
      headers[key.toLowerCase()] = value;
    }
  }

  return { statusCode, statusText, headers, body: bodyBytes };
}

/**
 * Dechunks a Transfer-Encoding: chunked body.
 */
function dechunkBuffer(data: Uint8Array): Uint8Array {
  const text = new TextDecoder().decode(data);
  const chunks: Uint8Array[] = [];
  let pos = 0;

  while (pos < text.length) {
    const lineEnd = text.indexOf("\r\n", pos);
    if (lineEnd === -1) break;

    const sizeStr = text.slice(pos, lineEnd).trim();
    const size = parseInt(sizeStr, 16);
    if (isNaN(size) || size === 0) break;

    const chunkStart = lineEnd + 2;
    const chunkData = new TextEncoder().encode(text.slice(chunkStart, chunkStart + size));
    chunks.push(chunkData);

    pos = chunkStart + size + 2; // skip chunk data + \r\n
  }

  const totalLength = chunks.reduce((sum, c) => sum + c.length, 0);
  const result = new Uint8Array(totalLength);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.length;
  }
  return result;
}

/**
 * Common handler for both local service and Vercel function.
 * Returns true if the request was handled, false if it should be passed through.
 */
export async function handleRequest(
  req: RequestInfo,
  res: ResponseWriter
): Promise<boolean> {
  console.log(`Incoming request: ${req.method} ${req.url}`);

  // Health check endpoint
  if (req.url === "/__bridge/health") {
    res.status(200).send("dispatcher ok");
    return true;
  }

  // Tunnel connect endpoint - called by the bridge server to trigger tunnel connection
  if (req.url === "/__tunnel/connect" && req.method === "POST") {
    try {
      await getTunnelClient();
      res.status(200).json({ status: "connected" });
    } catch (error) {
      console.error("Failed to establish tunnel connection:", error);
      res.status(503).json({
        error: "Service Unavailable",
        message: error instanceof Error ? error.message : String(error),
      });
    }
    return true;
  }

  // Status endpoint - check if tunnel is connected
  if (req.url === "/__tunnel/status") {
    const client = getCurrentTunnelClient();
    if (client && client.connected) {
      res.status(200).json({
        status: "connected",
        message: "Dispatcher tunnel active",
      });
    } else {
      res.status(200).json({
        status: "disconnected",
        message: "No active tunnel connection",
      });
    }
    return true;
  }

  // Proxy all other requests through the tunnel, retry once on stale connection
  const maxAttempts = 2;
  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      const client = await getTunnelClient();

      const rawRequest = buildHttpRequest(req);

      console.log(`[DEBUG] Forwarding request: ${req.method} ${req.url}`);
      const response = await client.forwardRequest(
        rawRequest,
        { ip: req.remoteIp ?? "127.0.0.1", port: req.remotePort ?? 0 },
        { ip: req.localIp ?? "127.0.0.1", port: req.localPort ?? 0 }
      );

      console.log(`[DEBUG] Got tunnel response: dataLen=${response.data.length}`);
      const parsed = parseHttpResponse(response.data);
      console.log(`[DEBUG] Parsed HTTP response: status=${parsed.statusCode} bodyLen=${parsed.body.length} headers=${JSON.stringify(parsed.headers)}`);

      // Set response headers
      for (const [key, value] of Object.entries(parsed.headers)) {
        if (key === "transfer-encoding") continue; // we dechunk
        res.setHeader(key, value);
      }

      // Dechunk body if needed
      let bodyBytes = parsed.body;
      if (parsed.headers["transfer-encoding"]?.includes("chunked")) {
        bodyBytes = dechunkBuffer(bodyBytes);
      }

      console.log(`[DEBUG] Sending response: status=${parsed.statusCode} bodyLen=${bodyBytes.length}`);
      res.status(parsed.statusCode).send(new TextDecoder().decode(bodyBytes));
      console.log(`[DEBUG] Response sent successfully`);
      break;
    } catch (error) {
      console.error(`Proxy error (attempt ${attempt}/${maxAttempts}):`, error);
      resetTunnelClient();

      // Retry on any tunnel/connection error
      if (attempt < maxAttempts) {
        console.log("Retrying with fresh connection...");
        continue;
      }

      const isConnectionError = error instanceof Error &&
        (error.message.includes("BRIDGE_SERVER_ADDR") || error.message.includes("timed out") || error.message.includes("connect") || error.message.includes("WebSocket"));

      res.status(isConnectionError ? 503 : 502).json({
        error: isConnectionError ? "Service Unavailable" : "Bad Gateway",
        message: error instanceof Error ? error.message : String(error),
      });
      break;
    }
  }

  return true;
}
