import { NextRequest, NextResponse } from "next/server";
import { getTodo, updateTodo, deleteTodo } from "../store";

function parseId(params: { id: string }): number | null {
  const id = Number(params.id);
  return isNaN(id) ? null : id;
}

export async function GET(
  _req: NextRequest,
  { params }: { params: Promise<{ id: string }> }
) {
  const { id: rawId } = await params;
  const id = parseId({ id: rawId });
  if (id === null) {
    return NextResponse.json({ error: "Invalid todo ID" }, { status: 400 });
  }
  const todo = getTodo(id);
  if (!todo) {
    return NextResponse.json({ error: "Todo not found" }, { status: 404 });
  }
  return NextResponse.json(todo);
}

export async function PUT(
  req: NextRequest,
  { params }: { params: Promise<{ id: string }> }
) {
  const { id: rawId } = await params;
  const id = parseId({ id: rawId });
  if (id === null) {
    return NextResponse.json({ error: "Invalid todo ID" }, { status: 400 });
  }
  const body = await req.json();
  const todo = updateTodo(id, body);
  if (!todo) {
    return NextResponse.json({ error: "Todo not found" }, { status: 404 });
  }
  return NextResponse.json(todo);
}

export async function DELETE(
  _req: NextRequest,
  { params }: { params: Promise<{ id: string }> }
) {
  const { id: rawId } = await params;
  const id = parseId({ id: rawId });
  if (id === null) {
    return NextResponse.json({ error: "Invalid todo ID" }, { status: 400 });
  }
  if (!deleteTodo(id)) {
    return NextResponse.json({ error: "Todo not found" }, { status: 404 });
  }
  return NextResponse.json({ message: "Todo deleted" });
}
