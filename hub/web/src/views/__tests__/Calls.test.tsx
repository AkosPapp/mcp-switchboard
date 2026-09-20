import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";

import Calls from "../Calls";
import { callFixture, renderView, statsFixture, stubHub } from "./helpers";

const first = callFixture();
const second = callFixture({
  id: "call-2",
  tool: "write_file",
  exposedName: "laptop__files__write_file",
  status: "error",
  result: null,
  error: "PermissionError: '/etc' is outside the allowed root",
  durationMs: 1500,
  source: "console",
});

afterEach(() => vi.unstubAllGlobals());

/** The table and the card list are both in the DOM (one is hidden by CSS), so
 * assertions scope themselves to one of them. */
function table() {
  return within(screen.getByRole("table"));
}

describe("Calls", () => {
  it("lists the calls the hub returns", async () => {
    stubHub({ calls: { calls: [first, second] }, stats: statsFixture });
    renderView(<Calls />, "/calls");

    await waitFor(() => expect(table().getByText("read_file")).toBeTruthy());
    expect(table().getByText("write_file")).toBeTruthy();
    expect(table().getAllByText("laptop")).toHaveLength(2);
    expect(table().getByText("1.50 s")).toBeTruthy();
  });

  it("shows the call counts from the hub's stats", async () => {
    stubHub({ calls: { calls: [first] }, stats: statsFixture });
    renderView(<Calls />, "/calls");

    expect(await screen.findByText("2 calls · 1 ok · 1 error")).toBeTruthy();
  });

  it("opens the detail pane for a clicked call", async () => {
    stubHub({
      calls: { calls: [first, second] },
      stats: statsFixture,
      "calls/call-2": second,
    });
    renderView(<Calls />, "/calls");

    await waitFor(() => expect(table().getByText("write_file")).toBeTruthy());
    fireEvent.click(table().getByText("write_file"));

    const detail = within(await screen.findByRole("complementary", { name: "Call detail" }));
    expect(detail.getByText("laptop__files__write_file")).toBeTruthy();
    expect(
      detail.getByText("PermissionError: '/etc' is outside the allowed root"),
    ).toBeTruthy();
    expect(detail.getByText("call-2")).toBeTruthy();

    fireEvent.click(detail.getByRole("button", { name: "Close call detail" }));
    expect(screen.queryByRole("complementary", { name: "Call detail" })).toBeNull();
  });

  it("puts applied filters in the query string", async () => {
    stubHub({ calls: { calls: [first] }, stats: statsFixture });
    renderView(<Calls />, "/calls");

    await waitFor(() => expect(table().getByText("read_file")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("Machine"), { target: { value: "laptop" } });
    fireEvent.change(screen.getByLabelText("Status"), { target: { value: "error" } });
    fireEvent.change(screen.getByLabelText("Limit"), { target: { value: "250" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));

    await waitFor(() => {
      const search = new URLSearchParams(screen.getByTestId("search").textContent ?? "");
      expect(search.get("label")).toBe("laptop");
      expect(search.get("status")).toBe("error");
      expect(search.get("limit")).toBe("250");
    });
  });

  it("starts from the filters already in the query string", async () => {
    const fetchMock = stubHub({ calls: { calls: [first] }, stats: statsFixture });
    renderView(<Calls />, "/calls?tool=read_file&limit=50");

    await waitFor(() =>
      expect(
        fetchMock.mock.calls.some(([url]) => {
          const text = String(url);
          return text.includes("tool=read_file") && text.includes("limit=50");
        }),
      ).toBe(true),
    );
    expect((screen.getByLabelText("Tool") as HTMLInputElement).value).toBe("read_file");
  });

  it("clears the filters on reset", async () => {
    stubHub({ calls: { calls: [first] }, stats: statsFixture });
    renderView(<Calls />, "/calls?tool=read_file");

    fireEvent.click(await screen.findByRole("button", { name: "Reset" }));

    await waitFor(() => expect(screen.getByTestId("search").textContent).toBe(""));
  });

  it("says so when nothing has been called yet", async () => {
    stubHub({ calls: { calls: [] }, stats: statsFixture });
    renderView(<Calls />, "/calls");

    expect(await screen.findByText("No calls recorded yet.")).toBeTruthy();
  });

  it("shows the hub's message when the call log fails to load", async () => {
    stubHub(
      { stats: statsFixture },
      { errors: { calls: { status: 500, detail: "store is locked" } } },
    );
    renderView(<Calls />, "/calls");

    expect(await screen.findByText(/store is locked/)).toBeTruthy();
  });
});
