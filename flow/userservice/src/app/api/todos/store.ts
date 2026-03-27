export interface Todo {
  id: number;
  title: string;
  completed: boolean;
  created_at: string;
}

const todos: Todo[] = [];
let nextId = 1;

export function listTodos(): Todo[] {
  return todos;
}

export function getTodo(id: number): Todo | undefined {
  return todos.find((t) => t.id === id);
}

export function createTodo(title: string): Todo {
  const todo: Todo = {
    id: nextId++,
    title,
    completed: false,
    created_at: new Date().toISOString(),
  };
  todos.push(todo);
  return todo;
}

export function updateTodo(
  id: number,
  fields: { title?: string; completed?: boolean }
): Todo | undefined {
  const todo = todos.find((t) => t.id === id);
  if (!todo) return undefined;
  if (fields.title !== undefined) todo.title = fields.title;
  if (fields.completed !== undefined) todo.completed = fields.completed;
  return todo;
}

export function deleteTodo(id: number): boolean {
  const idx = todos.findIndex((t) => t.id === id);
  if (idx === -1) return false;
  todos.splice(idx, 1);
  return true;
}
