import type {VercelRequest, VercelResponse} from "@vercel/node";
import {waitUntil} from "@vercel/functions";
import {handleRequest, type ResponseWriter} from "../src/common-handler.js";
import {getCurrentTunnelClient} from "../src/connection.js";

/**
 * Adapts VercelResponse to ResponseWriter interface
 */
function adaptResponse(res: VercelResponse): ResponseWriter {
  return {
    status(code: number) {
      res.status(code);
      return this;
    },
    setHeader(name: string, value: string) {
      res.setHeader(name, value);
    },
    send(body: string) {
      res.send(body);
    },
    json(body: object) {
      res.json(body);
    },
  };
}

export default async function handler(req: VercelRequest, res: VercelResponse) {
  // Get body as string for POST requests
  let body: string | undefined;
  if (req.method === "POST" && req.body) {
    body = typeof req.body === "string" ? req.body : JSON.stringify(req.body);
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

  // Resolve source IP from x-forwarded-for or socket
  const forwardedFor = req.headers["x-forwarded-for"];
  const remoteIp = typeof forwardedFor === "string"
    ? forwardedFor.split(",")[0].trim()
    : req.socket?.remoteAddress;

  await handleRequest(
    {
      method: req.method || "GET",
      url: req.url || "/",
      headers,
      body,
      remoteIp,
      remotePort: req.socket?.remotePort,
    },
    adaptResponse(res)
  );

  // Keep the Vercel function alive while the WebSocket processes messages
  const client = getCurrentTunnelClient();
  if (client) {
    waitUntil(client.runUntilDone());
  }
}
