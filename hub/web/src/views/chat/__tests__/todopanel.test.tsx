import { fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { renderView, stubHub } from "../../__tests__/helpers";
import TodoPanel from "../TodoPanel";

afterEach(() => {
  vi.unstubAllGlobals();
  window.localStorage.clear();
});

const todos = [
  { id: 1, content: "Read the code", status: "completed" },
  { id: 2, content: "Write the panel", status: "in_progress" },
  { id: 3, content: "Test it", status: "pending" },
];

describe("TodoPanel", () => {
  it("is hidden while the plan is empty", async () => {
    const fetchMock = stubHub({ "chats/c1/todos": { todos: [] } });
    renderView(<TodoPanel chatId="c1" />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(screen.queryByTestId("todo-panel")).toBeNull();
  });

  it("lists items with a status icon each and collapses", async () => {
    stubHub({ "chats/c1/todos": { todos } });
    renderView(<TodoPanel chatId="c1" />);
    expect(await screen.findByText("Write the panel")).toBeTruthy();
    expect(screen.getByText("1/3")).toBeTruthy();
    expect(screen.getByLabelText("completed")).toBeTruthy();
    expect(screen.getByLabelText("in progress")).toBeTruthy();
    expect(screen.getByLabelText("pending")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /plan/i }));
    // Collapsed: only the current item is echoed in the header.
    expect(screen.queryByLabelText("pending")).toBeNull();
    expect(screen.getByText("Write the panel")).toBeTruthy();
  });
});
