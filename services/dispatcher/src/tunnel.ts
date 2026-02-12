import {create, toBinary, fromBinary} from "@bufbuild/protobuf";
import {
  MessageSchema,
  Message_RegistrationSchema,
  Message_AddressSchema,
  Message_Protocol,
  ResolveDNSQueryResponseSchema,
  type Message,
} from "@vercel/bridge-api";
import {randomUUID} from "crypto";
import * as dns from "dns";
import * as net from "net";
import WebSocket from "ws";
import {logger} from "./logger.js";

export interface TunnelConfig {
  serverAddr: string;
}

interface PendingRequest {
  resolve: (response: Message) => void;
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
    // Clean up old connection if any
    if (this.ws) {
      this.ws.removeAllListeners();
      try { this.ws.close(); } catch {}
      this.ws = null;
    }
    this.isConnected = false;

    // Convert HTTP URL to WebSocket URL
    let wsUrl = this.config.serverAddr;
    if (wsUrl.startsWith("https://")) {
      wsUrl = "wss://" + wsUrl.slice(8);
    } else if (wsUrl.startsWith("http://")) {
      wsUrl = "ws://" + wsUrl.slice(7);
    }

    // Ensure /tunnel path for WebSocket endpoint
    if (!wsUrl.endsWith("/tunnel")) {
      wsUrl = wsUrl.replace(/\/$/, "") + "/tunnel";
    }

    logger.debug(`Connecting to WebSocket: ${wsUrl}`);

    return new Promise<void>((resolve, reject) => {
      this.ws = new WebSocket(wsUrl, {
        handshakeTimeout: 30000,
      });

      this.ws.binaryType = "nodebuffer";

      this.ws.on("open", () => {
        logger.debug("WebSocket connection established");
        this.isConnected = true;

        // Send registration message
        const registration = create(Message_RegistrationSchema, {
          isServer: true,
          protocol: Message_Protocol.TCP,
        });

        const registrationMsg = create(MessageSchema, {
          registration,
          data: new Uint8Array(),
          connectionId: "",
          close: false,
        });

        this.sendMessageDirect(registrationMsg);

        // Start processing incoming messages
        this.processingPromise = new Promise<void>((doneResolve) => {
          this.doneResolve = doneResolve;
        });

        resolve();
      });

      this.ws.on("message", (data: Buffer) => {
        try {
          const message = fromBinary(MessageSchema, new Uint8Array(data));
          this.handleIncomingMessage(message);
        } catch (err) {
          logger.error("Failed to parse incoming message:", err);
        }
      });

      this.ws.on("close", () => {
        logger.debug("WebSocket connection closed");
        this.isConnected = false;
        // Reject pending HTTP requests — they'll be retried by the caller
        for (const [, pending] of this.pendingRequests) {
          pending.reject(new Error("WebSocket connection closed"));
        }
        this.pendingRequests.clear();
        // Don't destroy outgoing connections — they may resume after reconnect
        if (this.doneResolve) {
          this.doneResolve();
        }
      });

      this.ws.on("error", (err) => {
        logger.error("WebSocket error:", err);
        if (!this.isConnected) {
          reject(err);
        }
      });

      // Setup ping interval to keep connection alive
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

  /**
   * Ensures the WebSocket is connected, reconnecting if necessary.
   * Deduplicates concurrent reconnection attempts.
   */
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

  /**
   * Send a message directly without reconnection (used during connect handshake).
   */
  private sendMessageDirect(msg: Message): boolean {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      return false;
    }
    const binary = toBinary(MessageSchema, msg);
    this.ws.send(binary);
    return true;
  }

  /**
   * Send a message, reconnecting the WebSocket if necessary.
   */
  private async sendMessage(msg: Message): Promise<boolean> {
    if (!this.isConnected || !this.ws || this.ws.readyState !== WebSocket.OPEN) {
      try {
        await this.ensureConnected();
      } catch (err) {
        logger.error("Reconnection failed:", err);
        return false;
      }
    }

    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      logger.debug(`sendMessage failed: WebSocket not open connId=${msg.connectionId} dataLen=${msg.data?.length ?? 0} close=${msg.close}`);
      return false;
    }

    const binary = toBinary(MessageSchema, msg);
    logger.debug(`sendMessage: connId=${msg.connectionId} dataLen=${msg.data?.length ?? 0} close=${msg.close} dnsResponse=${!!msg.dnsResponse}`);
    this.ws.send(binary);
    return true;
  }

  /**
   * Returns a promise that resolves when the tunnel connection is done.
   * Use this with waitUntil to keep the function alive while processing messages.
   */
  async runUntilDone(): Promise<void> {
    if (this.processingPromise) {
      await this.processingPromise;
    }
  }

  /**
   * Cleans up a specific connection by ID.
   */
  private cleanupConnection(connectionId: string, error?: Error): void {
    // Clean up pending request if exists
    const pending = this.pendingRequests.get(connectionId);
    if (pending) {
      if (error) {
        pending.reject(error);
      }
      this.pendingRequests.delete(connectionId);
    }

    // Clean up outgoing connection if exists
    const conn = this.outgoingConnections.get(connectionId);
    if (conn) {
      conn.socket.destroy();
      this.outgoingConnections.delete(connectionId);
    }
  }

  private handleIncomingMessage(message: Message): void {
    // Handle DNS resolution requests
    if (message.dnsRequest) {
      this.handleDNSRequest(message.dnsRequest.requestId, message.dnsRequest.hostname);
      return;
    }

    const connectionId = message.connectionId;

    logger.debug(
      `Received message: connId=${connectionId} dataLen=${message.data.length} close=${message.close}`,
      message.source ? `src=${message.source.ip}:${message.source.port}` : "",
      message.dest ? `dst=${message.dest.ip}:${message.dest.port}` : ""
    );

    // Handle close message
    if (message.close) {
      logger.debug(`Connection closed: connId=${connectionId}`);

      // If there's a pending request, resolve it with accumulated data first
      const pending = this.pendingRequests.get(connectionId);
      if (pending) {
        const fullData = this.concatenateChunks(pending.chunks);
        const finalMessage = create(MessageSchema, {
          ...message,
          data: fullData,
        });
        pending.resolve(finalMessage);
        this.pendingRequests.delete(connectionId);
      }

      // Clean up the outgoing connection if it exists
      const conn = this.outgoingConnections.get(connectionId);
      if (conn) {
        conn.socket.destroy();
        this.outgoingConnections.delete(connectionId);
      }
      return;
    }

    // Check if this is a response to a pending request
    if (connectionId && this.pendingRequests.has(connectionId)) {
      const pending = this.pendingRequests.get(connectionId)!;
      // Accumulate data chunks
      if (message.data.length > 0) {
        pending.chunks.push(message.data);
      }
      return;
    }

    // This is outgoing traffic from dispatcher - make L4 connection
    this.handleOutgoingTraffic(message).catch((err) => {
      logger.error("Error handling outgoing traffic:", err);
    });
  }

  private async handleOutgoingTraffic(message: Message): Promise<void> {
    const connectionId = message.connectionId;
    const dest = message.dest;

    // Check if we already have a connection for this connectionId
    const conn = this.outgoingConnections.get(connectionId);

    if (message.close) {
      logger.debug(`Received close for connection: connId=${connectionId}`);
      this.cleanupConnection(connectionId);
      return;
    }

    // Existing connection — forward or buffer data
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

    // New connection requires a destination
    if (!dest) {
      logger.error(`Outgoing message missing destination: connId=${connectionId}`);
      return;
    }

    // Register immediately to prevent duplicate connection attempts
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
          logger.debug(`Outgoing connection established: connId=${connectionId} dst=${dest.ip}:${dest.port}`);
          resolve();
        });
        socket.on("error", (err) => {
          clearTimeout(timeout);
          reject(err);
        });
      });
    } catch (err) {
      logger.error(`Failed to connect: connId=${connectionId} dst=${dest.ip}:${dest.port}`, err);
      this.cleanupConnection(connectionId, err instanceof Error ? err : new Error(String(err)));
      return;
    }

    newConn.connected = true;

    // Flush buffered data
    for (const buf of newConn.pendingData) {
      socket.write(buf);
    }
    newConn.pendingData = [];

    // Handle incoming data from the outgoing connection
    socket.on("data", (data) => {
      logger.debug(`Received response data: connId=${connectionId} dataLen=${data.length}`);
      const responseMsg = create(MessageSchema, {
        source: create(Message_AddressSchema, {ip: dest.ip, port: dest.port}),
        dest: message.source ? create(Message_AddressSchema, {
          ip: message.source.ip,
          port: message.source.port
        }) : undefined,
        data: new Uint8Array(data),
        connectionId,
        close: false,
      });
      this.sendMessage(responseMsg).catch((err) => {
        logger.error(`Failed to send response data: connId=${connectionId}`, err);
      });
    });

    socket.on("close", () => {
      logger.debug(`Socket closed: connId=${connectionId}`);
      const closeMsg = create(MessageSchema, {
        source: create(Message_AddressSchema, {ip: dest.ip, port: dest.port}),
        dest: message.source ? create(Message_AddressSchema, {
          ip: message.source.ip,
          port: message.source.port
        }) : undefined,
        data: new Uint8Array(),
        connectionId,
        close: true,
      });
      this.sendMessage(closeMsg).catch((err) => {
        logger.debug(`Failed to send close message: connId=${connectionId}`, err);
      });
      this.outgoingConnections.delete(connectionId);
      this.pendingRequests.delete(connectionId);
    });

    // Send initial data to the destination
    if (message.data.length > 0) {
      socket.write(Buffer.from(message.data));
    }
  }

  private handleDNSRequest(requestId: string, hostname: string): void {
    logger.debug(`DNS resolution request: requestId=${requestId} hostname=${hostname}`);

    // Use dns.lookup() instead of dns.resolve4() so /etc/hosts entries are checked
    dns.lookup(hostname, {family: 4, all: true}, (err, results) => {
      const addresses = err ? [] : (results as dns.LookupAddress[]).map(r => r.address);
      const response = create(MessageSchema, {
        dnsResponse: create(ResolveDNSQueryResponseSchema, {
          requestId,
          addresses,
          error: err ? err.message : "",
        }),
      });
      this.sendMessage(response).catch((sendErr) => {
        logger.error(`Failed to send DNS response: requestId=${requestId}`, sendErr);
      });
      logger.debug(`DNS resolution response: requestId=${requestId} addresses=${addresses} error=${err?.message ?? ""}`);
    });
  }

  async forwardRequest(
    data: Uint8Array,
    source: { ip: string; port: number },
    dest: { ip: string; port: number }
  ): Promise<Message> {
    const connectionId = randomUUID();

    // Ensure connected before setting up the request
    await this.ensureConnected();

    return new Promise((resolve, reject) => {
      this.pendingRequests.set(connectionId, {
        resolve,
        reject,
        chunks: [],
      });

      const msg = create(MessageSchema, {
        source: create(Message_AddressSchema, {ip: source.ip, port: source.port}),
        dest: create(Message_AddressSchema, {ip: dest.ip, port: dest.port}),
        data,
        connectionId,
        close: false,
      });

      if (!this.sendMessageDirect(msg)) {
        this.pendingRequests.delete(connectionId);
        reject(new Error("WebSocket not connected"));
        return;
      }

      // Set timeout for request
      setTimeout(() => {
        if (this.pendingRequests.has(connectionId)) {
          this.pendingRequests.delete(connectionId);
          reject(new Error("Request timeout"));
        }
      }, 30000);
    });
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

  /**
   * Returns true if the tunnel connection is active and the WebSocket is open.
   */
  get connected(): boolean {
    return this.isConnected && this.ws !== null && this.ws.readyState === WebSocket.OPEN;
  }
}
