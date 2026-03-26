import type { VercelRequest, VercelResponse } from "@vercel/node";

interface User {
  id: number;
  email: string;
  username: string;
  first_name: string;
  last_name: string;
  is_active: boolean;
  created_at: string;
  updated_at: string;
}

// Shared store — same cold-start instance as index.ts when running locally,
// but in production each serverless invocation is isolated.
const users: User[] = [];

export default function handler(req: VercelRequest, res: VercelResponse) {
  const userId = Number(req.query.id);
  if (isNaN(userId)) {
    return res.status(400).json({ error: "Invalid user ID" });
  }

  const idx = users.findIndex((u) => u.id === userId);
  if (idx === -1) {
    return res.status(404).json({ error: "User not found" });
  }

  if (req.method === "GET") {
    return res.json(users[idx]);
  }

  if (req.method === "PUT") {
    const { email, username, first_name, last_name, is_active } =
      req.body ?? {};

    if (email !== undefined && users.some((u) => u.id !== userId && u.email === email)) {
      return res.status(409).json({ error: "Email already exists" });
    }
    if (username !== undefined && users.some((u) => u.id !== userId && u.username === username)) {
      return res.status(409).json({ error: "Username already exists" });
    }

    const user = users[idx];
    if (email !== undefined) user.email = email;
    if (username !== undefined) user.username = username;
    if (first_name !== undefined) user.first_name = first_name;
    if (last_name !== undefined) user.last_name = last_name;
    if (is_active !== undefined) user.is_active = is_active;
    user.updated_at = new Date().toISOString();

    return res.json(user);
  }

  if (req.method === "DELETE") {
    users.splice(idx, 1);
    return res.json({ message: "User deleted" });
  }

  res.setHeader("Allow", "GET, PUT, DELETE");
  return res.status(405).json({ error: "Method not allowed" });
}
