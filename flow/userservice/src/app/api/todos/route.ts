import { NextRequest, NextResponse } from "next/server";
import { listTodos, createTodo } from "./stores";

export function GET() {
  return NextResponse.json({ todos: listTodos() });
}

export async function POST(req: NextRequest) {
  const body = await req.json();
  const { title } = body;

  if (!title || typeof title !== "string") {
    return NextResponse.json(
      { error: "Missing required field: title" },
      { status: 400 }
    );
  }

  const todo = createTodo(title);
  return NextResponse.json(todo, { status: 201 });
}
