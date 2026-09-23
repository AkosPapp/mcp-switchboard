import { describe, expect, it } from "vitest";

import { chatKeys } from "../../chat/api";
import { graphKeys } from "../hooks";

describe("query keys", () => {
  it("do not share the models key across views with different cached shapes", () => {
    expect(JSON.stringify(graphKeys.models)).not.toBe(JSON.stringify(chatKeys.models));
  });
});
