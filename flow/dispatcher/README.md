# Dispatcher

A Node server proxy running on Vercel intended to forward all incoming requests
to the `bridge server` running on the development Sandbox via Websocket (grpc marshalled messages
from [API](../../api/ts/bridge)).

1. On invocation, the function initiates a connection with the `bridge` server listed in the `REACH_SERVER_ADDR` env
   var.
2. Returns a response to the invocation.
3. All following requests are forwarded through the connection to `bridge`
4. All incoming messages from `bridge` that are a part of an incoming request are treated as responses to the received
   request (`connection_id` field in the `Connect` method).
5. All incoming message from `bridge` that are not part of an incoming request are treated as outgoing traffic from
   `dispatcher` itself. `dispatcher` then makes the outgoing L4 connection and returns all received data to `bridge`
