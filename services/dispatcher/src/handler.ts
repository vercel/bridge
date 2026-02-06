import type { IncomingMessage, ServerResponse } from "http";
import { handleRequest, type ResponseWriter } from "./common-handler.js";

/**
 * Adapts ServerResponse to ResponseWriter interface
 */
function adaptResponse(res: ServerResponse): ResponseWriter {
  let statusCode = 200;
  return {
    status(code: number) {
      statusCode = code;
      res.statusCode = code;
      return this;
    },
    setHeader(name: string, value: string) {
      res.setHeader(name, value);
    },
    send(body: string) {
      res.statusCode = statusCode;
      res.end(body);
    },
    json(body: object) {
      res.statusCode = statusCode;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify(body));
    },
  };
}

/**
 * Reads the request body as a string.
 */
async function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf-8")));
    req.on("error", reject);
  });
}

export async function handler(
  req: IncomingMessage,
  res: ServerResponse
): Promise<void> {
  // Read body for POST requests
  let body: string | undefined;
  if (req.method === "POST") {
    body = await readBody(req);
  }

  // Collect headers as flat record
  const headers: Record<string, string> = {};
  for (const [key, value] of Object.entries(req.headers)) {
    if (typeof value === "string") {
      headers[key] = value;
    } else if (Array.isArray(value)) {
      headers[key] = value.join(", ");
    }
  }

  await handleRequest(
    {
      method: req.method || "GET",
      url: req.url || "/",
      headers,
      body,
      remoteIp: req.socket.remoteAddress,
      remotePort: req.socket.remotePort,
    },
    adaptResponse(res)
  );
}
