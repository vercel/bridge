import {create, toBinary, fromBinary} from "@bufbuild/protobuf";
import {
  TunnelNetworkMessageSchema,
  TunnelAddressSchema,
  TunnelProtocol,
  type TunnelNetworkMessage,
} from "@vercel/bridge-api/bridge/v1/proxy_pb";
import * as dns from "dns";
import * as net from "net";
import WebSocket from "ws";
import {logger} from "./logger.js";

export interface TunnelConfig {
  serverAddr: string;
}

interface PendingRequest {
  resolve: (response: TunnelNetworkMessage) => void;
  reject: (error: Error) => void;
  chunks: Uint8Array[];
}

interface OutgoingConnection {
  socket: net.Socket;
  connectionId: string;
  connected: boolean;
  pendingData: Buffer[];
}

export class TunnelClient {
  private config: TunnelConfig;
  private ws: WebSocket | null = null;
  private pendingRequests = new Map<string, PendingRequest>();
  private outgoingConnections = new Map<string, OutgoingConnection>();
  private isConnected = false;
  private processingPromise: Promise<void> | null = null;
  private doneResolve: (() => void) | null = null;
  private reconnectPromise: Promise<void> | null = null;

  constructor(config: TunnelConfig) {
    this.config = config;
  }

  async connect(): Promise<void> {
    if (this.ws) {
      this.ws.removeAllListeners();
      try { this.ws.close(); } catch {}
      this.ws = null;
    }
    this.isConnected = false;

    // Build WebSocket URL pointing to the interceptor tunnel endpoint.
    let wsUrl = this.config.serverAddr;
    if (wsUrl.startsWith("https://")) {
      wsUrl = "wss://" + wsUrl.slice(8);
    } else if (wsUrl.startsWith("http://")) {
      wsUrl = "ws://" + wsUrl.slice(7);
    }
    wsUrl = wsUrl.replace(/\/$/, "") + "/__tunnel/connect";

    console.log(`[tunnel] connecting to WebSocket: ${wsUrl}`);

    return new Promise<void>((resolve, reject) => {
      this.ws = new WebSocket(wsUrl, {
        handshakeTimeout: 30000,
      });

      this.ws.binaryType = "nodebuffer";

      this.ws.on("open", () => {
        console.log(`[tunnel] WebSocket connection established to ${wsUrl}`);
        this.isConnected = true;

        this.processingPromise = new Promise<void>((doneResolve) => {
          this.doneResolve = doneResolve;
        });

        resolve();
      });

      this.ws.on("message", (data: Buffer) => {
        try {
          const message = fromBinary(TunnelNetworkMessageSchema, new Uint8Array(data));
          this.handleIncomingMessage(message);
        } catch (err) {
          logger.error("Failed to parse incoming message:", err);
        }
      });

      this.ws.on("close", () => {
        console.log(`[tunnel] WebSocket connection closed`);
        this.isConnected = false;
        for (const [, pending] of this.pendingRequests) {
          pending.reject(new Error("WebSocket connection closed"));
        }
        this.pendingRequests.clear();
        if (this.doneResolve) {
          this.doneResolve();
        }
      });

      this.ws.on("error", (err) => {
        console.error(`[tunnel] WebSocket error (connected=${this.isConnected}):`, err);
        if (!this.isConnected) {
          reject(err);
        }
      });

      const pingInterval = setInterval(() => {
        if (this.ws && this.ws.readyState === WebSocket.OPEN) {
          this.ws.ping();
        } else {
          clearInterval(pingInterval);
        }
      }, 30000);

      this.ws.on("close", () => clearInterval(pingInterval));
    });
  }

  private async ensureConnected(): Promise<void> {
    if (this.isConnected && this.ws?.readyState === WebSocket.OPEN) {
      return;
    }

    if (this.reconnectPromise) {
      return this.reconnectPromise;
    }

    logger.info("WebSocket disconnected, reconnecting...");
    this.reconnectPromise = this.connect().finally(() => {
      this.reconnectPromise = null;
    });

    return this.reconnectPromise;
  }

  private sendMessageDirect(msg: TunnelNetworkMessage): boolean {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      return false;
    }
    const binary = toBinary(TunnelNetworkMessageSchema, msg);
    this.ws.send(binary);
    return true;
  }

  private async sendMessage(msg: TunnelNetworkMessage): Promise<boolean> {
    if (!this.isConnected || !this.ws || this.ws.readyState !== WebSocket.OPEN) {
      try {
        await this.ensureConnected();
      } catch (err) {
        logger.error("Reconnection failed:", err);
        return false;
      }
    }

    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      logger.debug(`sendMessage failed: WebSocket not open connId=${msg.connectionId}`);
      return false;
    }

    const binary = toBinary(TunnelNetworkMessageSchema, msg);
    this.ws.send(binary);
    return true;
  }

  /**
   * Returns a promise that resolves when the tunnel connection is done.
   * Use with waitUntil to keep the Vercel function alive.
   */
  async runUntilDone(): Promise<void> {
    if (this.processingPromise) {
      await this.processingPromise;
    }
  }

  private cleanupConnection(connectionId: string, error?: Error): void {
    const pending = this.pendingRequests.get(connectionId);
    if (pending) {
      if (error) {
        pending.reject(error);
      }
      this.pendingRequests.delete(connectionId);
    }

    const conn = this.outgoingConnections.get(connectionId);
    if (conn) {
      conn.socket.destroy();
      this.outgoingConnections.delete(connectionId);
    }
  }

  private handleIncomingMessage(message: TunnelNetworkMessage): void {
    const connectionId = message.connectionId;

    logger.debug(
      `Received message: connId=${connectionId} dataLen=${message.data.length}`,
      message.source ? `src=${message.source.ip}:${message.source.port}` : "",
      message.dest ? `dst=${message.dest.ip}:${message.dest.port}` : ""
    );

    // An error field signals the remote side closed or failed this connection.
    if (message.error) {
      logger.debug(`Connection error: connId=${connectionId} error=${message.error}`);

      const pending = this.pendingRequests.get(connectionId);
      if (pending) {
        const fullData = this.concatenateChunks(pending.chunks);
        const finalMessage = create(TunnelNetworkMessageSchema, {
          ...message,
          data: fullData,
        });
        pending.resolve(finalMessage);
        this.pendingRequests.delete(connectionId);
      }

      const conn = this.outgoingConnections.get(connectionId);
      if (conn) {
        conn.socket.destroy();
        this.outgoingConnections.delete(connectionId);
      }
      return;
    }

    // Accumulate chunks for pending HTTP request/response.
    if (connectionId && this.pendingRequests.has(connectionId)) {
      const pending = this.pendingRequests.get(connectionId)!;
      if (message.data.length > 0) {
        pending.chunks.push(message.data);
      }

      if (this.isCompleteHttpResponse(pending.chunks)) {
        const fullData = this.concatenateChunks(pending.chunks);
        const finalMessage = create(TunnelNetworkMessageSchema, {
          connectionId,
          data: fullData,
        });
        pending.resolve(finalMessage);
        this.pendingRequests.delete(connectionId);

        const closeMsg = create(TunnelNetworkMessageSchema, {
          connectionId,
          error: "dispatcher: response complete",
          data: new Uint8Array(),
        });
        this.sendMessageDirect(closeMsg);
      }
      return;
    }

    // Outgoing traffic — make L4 connection to the destination.
    this.handleOutgoingTraffic(message).catch((err) => {
      logger.error("Error handling outgoing traffic:", err);
    });
  }

  private async handleOutgoingTraffic(message: TunnelNetworkMessage): Promise<void> {
    const connectionId = message.connectionId;
    const dest = message.dest;

    if (message.error) {
      this.cleanupConnection(connectionId);
      return;
    }

    // Existing connection — forward or buffer data.
    const conn = this.outgoingConnections.get(connectionId);
    if (conn) {
      if (message.data.length > 0) {
        if (conn.connected) {
          conn.socket.write(Buffer.from(message.data));
        } else {
          conn.pendingData.push(Buffer.from(message.data));
        }
      }
      return;
    }

    if (!dest) {
      logger.error(`Outgoing message missing destination: connId=${connectionId}`);
      return;
    }

    // New connection.
    const socket = new net.Socket();
    const newConn: OutgoingConnection = {
      socket,
      connectionId,
      connected: false,
      pendingData: message.data.length > 0 ? [Buffer.from(message.data)] : [],
    };
    this.outgoingConnections.set(connectionId, newConn);

    logger.debug(`Creating outgoing connection: connId=${connectionId} dst=${dest.ip}:${dest.port}`);

    try {
      await new Promise<void>((resolve, reject) => {
        const timeout = setTimeout(() => {
          reject(new Error(`Connection timed out to ${dest.ip}:${dest.port}`));
        }, 10000);
        socket.connect(dest.port, dest.ip, () => {
          clearTimeout(timeout);
          resolve();
        });
        socket.on("error", (err) => {
          clearTimeout(timeout);
          reject(err);
        });
      });
    } catch (err) {
      logger.error(`Failed to connect: connId=${connectionId} dst=${dest.ip}:${dest.port}`, err);
      // Send error back through tunnel.
      const errMsg = create(TunnelNetworkMessageSchema, {
        connectionId,
        error: err instanceof Error ? err.message : String(err),
        data: new Uint8Array(),
      });
      this.sendMessage(errMsg).catch(() => {});
      this.cleanupConnection(connectionId, err instanceof Error ? err : new Error(String(err)));
      return;
    }

    newConn.connected = true;

    for (const buf of newConn.pendingData) {
      socket.write(buf);
    }
    newConn.pendingData = [];

    socket.on("data", (data) => {
      const responseMsg = create(TunnelNetworkMessageSchema, {
        source: create(TunnelAddressSchema, {ip: dest.ip, port: dest.port}),
        dest: message.source ? create(TunnelAddressSchema, {
          ip: message.source.ip,
          port: message.source.port,
        }) : undefined,
        data: new Uint8Array(data),
        connectionId,
        protocol: TunnelProtocol.TCP,
      });
      this.sendMessage(responseMsg).catch((err) => {
        logger.error(`Failed to send response data: connId=${connectionId}`, err);
      });
    });

    socket.on("close", () => {
      const errMsg = create(TunnelNetworkMessageSchema, {
        connectionId,
        error: "connection closed",
        data: new Uint8Array(),
      });
      this.sendMessage(errMsg).catch(() => {});
      this.outgoingConnections.delete(connectionId);
      this.pendingRequests.delete(connectionId);
    });
  }

  /**
   * Resolves a hostname using the local system DNS resolver.
   * Called by the intercept client via a tunnel message.
   */
  resolveDNS(hostname: string): Promise<string[]> {
    return new Promise((resolve) => {
      dns.lookup(hostname, {family: 4, all: true}, (err, results) => {
        if (err) {
          logger.debug(`DNS resolution failed: hostname=${hostname} error=${err.message}`);
          resolve([]);
          return;
        }
        const addresses = (results as dns.LookupAddress[]).map(r => r.address);
        logger.debug(`DNS resolved: hostname=${hostname} addresses=${addresses}`);
        resolve(addresses);
      });
    });
  }

  /**
   * Forwards an HTTP request through the tunnel and waits for the response.
   */
  async forwardRequest(
    data: Uint8Array,
    source: { ip: string; port: number },
    dest: { ip: string; port: number }
  ): Promise<TunnelNetworkMessage> {
    const connectionId = `${source.ip}:${source.port}->${dest.ip}:${dest.port}`;

    await this.ensureConnected();

    logger.debug(`forwardRequest: connId=${connectionId} dataLen=${data.length}`);

    return new Promise((resolve, reject) => {
      this.pendingRequests.set(connectionId, {
        resolve,
        reject,
        chunks: [],
      });

      const msg = create(TunnelNetworkMessageSchema, {
        source: create(TunnelAddressSchema, {ip: source.ip, port: source.port}),
        dest: create(TunnelAddressSchema, {ip: dest.ip, port: dest.port}),
        data,
        connectionId,
        protocol: TunnelProtocol.TCP,
      });

      if (!this.sendMessageDirect(msg)) {
        this.pendingRequests.delete(connectionId);
        reject(new Error("WebSocket not connected"));
        return;
      }

      setTimeout(() => {
        if (this.pendingRequests.has(connectionId)) {
          this.pendingRequests.delete(connectionId);
          reject(new Error("Request timeout"));
        }
      }, 30000);
    });
  }

  private isCompleteHttpResponse(chunks: Uint8Array[]): boolean {
    const data = this.concatenateChunks(chunks);
    const text = new TextDecoder().decode(data);

    const headerEnd = text.indexOf("\r\n\r\n");
    if (headerEnd === -1) return false;

    const headerSection = text.slice(0, headerEnd).toLowerCase();
    const bodyStartIdx = headerEnd + 4;

    const clMatch = headerSection.match(/content-length:\s*(\d+)/);
    if (clMatch) {
      const expectedLen = parseInt(clMatch[1], 10);
      const bodyBytes = data.slice(new TextEncoder().encode(text.slice(0, bodyStartIdx)).length);
      return bodyBytes.length >= expectedLen;
    }

    if (headerSection.includes("transfer-encoding: chunked")) {
      return text.endsWith("0\r\n\r\n") || text.includes("\r\n0\r\n\r\n");
    }

    return false;
  }

  private concatenateChunks(chunks: Uint8Array[]): Uint8Array {
    const totalLength = chunks.reduce((sum, chunk) => sum + chunk.length, 0);
    const result = new Uint8Array(totalLength);
    let offset = 0;
    for (const chunk of chunks) {
      result.set(chunk, offset);
      offset += chunk.length;
    }
    return result;
  }

  disconnect(): void {
    this.isConnected = false;
    if (this.ws) {
      this.ws.removeAllListeners();
      this.ws.close();
      this.ws = null;
    }
    for (const conn of this.outgoingConnections.values()) {
      conn.socket.destroy();
    }
    this.outgoingConnections.clear();
    this.pendingRequests.clear();
  }

  get connected(): boolean {
    return this.isConnected && this.ws !== null && this.ws.readyState === WebSocket.OPEN;
  }
}
