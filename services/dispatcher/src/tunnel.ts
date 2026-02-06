import {create, toBinary, fromBinary} from "@bufbuild/protobuf";
import {
  MessageSchema,
  Message_RegistrationSchema,
  Message_AddressSchema,
  Message_Protocol,
  type Message,
} from "@vercel/bridge-api";
import {randomUUID} from "crypto";
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
}

export class TunnelClient {
  private config: TunnelConfig;
  private ws: WebSocket | null = null;
  private pendingRequests = new Map<string, PendingRequest>();
  private outgoingConnections = new Map<string, OutgoingConnection>();
  private isConnected = false;
  private processingPromise: Promise<void> | null = null;
  private doneResolve: (() => void) | null = null;

  constructor(config: TunnelConfig) {
    this.config = config;
  }

  async connect(): Promise<void> {
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

        this.sendMessage(registrationMsg);

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
        this.cleanupAllConnections(new Error("WebSocket connection closed"));
        if (this.doneResolve) {
          this.doneResolve();
        }
      });

      this.ws.on("error", (err) => {
        logger.error("WebSocket error:", err);
        if (!this.isConnected) {
          reject(err);
        } else {
          this.cleanupAllConnections(err);
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

  private sendMessage(msg: Message): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      logger.error("Cannot send message: WebSocket not connected");
      return;
    }

    const binary = toBinary(MessageSchema, msg);
    this.ws.send(binary);
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

  private queueMessage(msg: Message): void {
    this.sendMessage(msg);
  }

  /**
   * Cleans up all pending requests and outgoing connections.
   */
  private cleanupAllConnections(error: Error): void {
    // Reject all pending requests
    for (const [connectionId, pending] of this.pendingRequests) {
      pending.reject(error);
    }
    this.pendingRequests.clear();

    // Close all outgoing connections
    for (const conn of this.outgoingConnections.values()) {
      conn.socket.destroy();
    }
    this.outgoingConnections.clear();
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

    if (!dest) {
      console.error("Outgoing message missing destination");
      return;
    }

    // Check if we already have a connection for this connectionId
    let conn = this.outgoingConnections.get(connectionId);

    if (message.close) {
      // Close the connection and clean up everything for this connectionId
      logger.debug(`Received close for connection: connId=${connectionId}`);
      this.cleanupConnection(connectionId);
      return;
    }

    if (!conn) {
      // Create new outgoing connection
      logger.debug(`Creating outgoing connection: connId=${connectionId} dst=${dest.ip}:${dest.port}`);
      const socket = new net.Socket();

      try {
        await new Promise<void>((resolve, reject) => {
          socket.connect(dest.port, dest.ip, () => {
            logger.debug(`Outgoing connection established: connId=${connectionId} dst=${dest.ip}:${dest.port}`);
            resolve();
          });
          socket.on("error", reject);
        });
      } catch (err) {
        logger.error(`Failed to connect: connId=${connectionId} dst=${dest.ip}:${dest.port}`, err);
        this.cleanupConnection(connectionId, err instanceof Error ? err : new Error(String(err)));
        return;
      }

      conn = {socket, connectionId};
      this.outgoingConnections.set(connectionId, conn);

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
        this.queueMessage(responseMsg);
      });

      socket.on("close", () => {
        logger.debug(`Socket closed: connId=${connectionId}`);
        // Send close message back through tunnel
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
        this.queueMessage(closeMsg);
        // Clean up this connection (but don't reject pending - close is normal)
        this.outgoingConnections.delete(connectionId);
        this.pendingRequests.delete(connectionId);
      });

      socket.on("error", (err) => {
        logger.error(`Outgoing connection error: connId=${connectionId}`, err);
        // Clean up and reject pending requests for this connection
        this.cleanupConnection(connectionId, err);
      });
    }

    // Send data to the destination
    if (message.data.length > 0) {
      conn.socket.write(Buffer.from(message.data));
    }
  }

  async forwardRequest(
    data: Uint8Array,
    source: { ip: string; port: number },
    dest: { ip: string; port: number }
  ): Promise<Message> {
    const connectionId = randomUUID();

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

      this.queueMessage(msg);

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
   * Returns true if the tunnel connection is active.
   */
  get connected(): boolean {
    return this.isConnected;
  }
}
