import {createServer} from "http";
import {handler} from "./handler.js";

const port = parseInt(process.env.PORT || "8080", 10);

const server = createServer(handler);

server.listen(port, () => {
  console.log(`Dispatcher server listening on port ${port}`);
});
