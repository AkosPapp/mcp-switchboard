import { describe, expect, it } from "vitest";

import { collect, fieldsOf, seed, unknownKeys } from "../schema";

const schema = {
  type: "object",
  properties: {
    message: { type: "string", description: "what to say" },
    times: { type: "integer" },
    ratio: { type: "number" },
    loud: { type: "boolean" },
    mode: { type: "string", enum: ["fast", "slow"] },
    nested: { type: "object", properties: { a: { type: "string" } } },
    list: { type: "array", items: { type: "string" } },
    nullable: { type: ["string", "null"] },
  },
  required: ["message"],
};

describe("fieldsOf", () => {
  it("renders primitives and skips what a form cannot represent", () => {
    const names = fieldsOf(schema).map((field) => field.name);
    expect(names).toEqual(["message", "times", "ratio", "loud", "mode", "nullable"]);
  });

  it("marks required fields and carries the description", () => {
    const fields = fieldsOf(schema);
    expect(fields.find((f) => f.name === "message")).toMatchObject({
      required: true,
      description: "what to say",
    });
    expect(fields.find((f) => f.name === "times")?.required).toBe(false);
  });

  it("treats an enum as an enum whatever its type says", () => {
    expect(fieldsOf(schema).find((f) => f.name === "mode")).toMatchObject({
      kind: "enum",
      options: ["fast", "slow"],
    });
  });

  it("reads a nullable union as its concrete type", () => {
    expect(fieldsOf(schema).find((f) => f.name === "nullable")?.kind).toBe("string");
  });

  it("is empty for a schema with no properties", () => {
    expect(fieldsOf(undefined)).toEqual([]);
    expect(fieldsOf({ type: "object" })).toEqual([]);
  });
});

describe("collect", () => {
  const fields = fieldsOf(schema);

  it("converts values to their declared types", () => {
    const { args, errors } = collect(fields, {
      message: "hi",
      times: "3",
      ratio: "1.5",
      loud: "true",
      mode: "fast",
    });
    expect(errors).toEqual([]);
    expect(args).toEqual({ message: "hi", times: 3, ratio: 1.5, loud: true, mode: "fast" });
  });

  // An absent optional argument must stay absent, so the tool applies its own
  // default rather than receiving an empty string.
  it("omits empty optional fields instead of sending them", () => {
    const { args } = collect(fields, { message: "hi", times: "" });
    expect(args).toEqual({ message: "hi" });
    expect("times" in args).toBe(false);
  });

  it("reports a missing required field", () => {
    const { errors } = collect(fields, {});
    expect(errors).toEqual(["message is required"]);
  });

  it("rejects a non-numeric number and a fractional integer", () => {
    expect(collect(fields, { message: "hi", times: "abc" }).errors).toContain(
      "times must be a number",
    );
    expect(collect(fields, { message: "hi", times: "1.5" }).errors).toContain(
      "times must be a whole number",
    );
  });
});

describe("seed and unknownKeys", () => {
  const fields = fieldsOf(schema);

  it("round-trips values back into the form", () => {
    expect(seed(fields, { message: "hi", times: 3, loud: true })).toEqual({
      message: "hi",
      times: "3",
      loud: "true",
    });
  });

  it("names the keys the form would drop", () => {
    expect(unknownKeys(fields, { message: "hi", nested: {} })).toEqual(["nested"]);
  });
});
