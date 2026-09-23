import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import Settings from "../Settings";
import { renderView, stubHub } from "./helpers";

describe("Settings", () => {
  it("disables the toggle and explains when the browser has no push support", async () => {
    // jsdom has no ServiceWorkerContainer/PushManager, so pushSupport()
    // reports "unsupported" without any stubbing - exercising the same path
    // a real unsupported browser takes.
    stubHub({});
    renderView(<Settings />);

    const toggle = await screen.findByRole("switch", { name: /notify me on this device/i });
    expect(toggle).toBeDisabled();
    expect(screen.getByText(/does not support push notifications/i)).toBeInTheDocument();
  });

  it("explains when the hub has no VAPID keys configured", async () => {
    // Simulate a supported browser by stubbing just enough of the API for
    // pushSupport() to report "available", then let the vapid-public-key
    // fetch answer 404 - exactly what the hub does with push disabled.
    Object.defineProperty(window, "PushManager", { value: function () {}, configurable: true });
    Object.defineProperty(navigator, "serviceWorker", {
      value: { register: async () => ({}), getRegistration: async () => undefined },
      configurable: true,
    });
    Object.defineProperty(window, "Notification", {
      value: { permission: "default" },
      configurable: true,
    });

    stubHub({}, { errors: { "push/vapid-public-key": { status: 404, detail: "not found" } } });
    renderView(<Settings />);

    await waitFor(() => {
      expect(screen.getByText(/not been configured for push notifications/i)).toBeInTheDocument();
    });
    const toggle = screen.getByRole("switch", { name: /notify me on this device/i });
    expect(toggle).toBeDisabled();
  });
});
