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

const users: User[] = [];
let nextId = 1;

export default function handler(req: VercelRequest, res: VercelResponse) {
  if (req.method === "GET") {
    return res.json({ users });
  }

  if (req.method === "POST") {
    const { email, username, first_name, last_name } = req.body ?? {};

    if (!email) {
      return res.status(400).json({ error: "Missing required field: email" });
    }
    if (!username) {
      return res
        .status(400)
        .json({ error: "Missing required field: username" });
    }

    if (users.some((u) => u.email === email)) {
      return res.status(409).json({ error: "Email already exists" });
    }
    if (users.some((u) => u.username === username)) {
      return res.status(409).json({ error: "Username already exists" });
    }

    const now = new Date().toISOString();
    const user: User = {
      id: nextId++,
      email,
      username,
      first_name: first_name ?? "",
      last_name: last_name ?? "",
      is_active: true,
      created_at: now,
      updated_at: now,
    };
    users.push(user);

    return res.status(201).json(user);
  }

  res.setHeader("Allow", "GET, POST");
  return res.status(405).json({ error: "Method not allowed" });
}
