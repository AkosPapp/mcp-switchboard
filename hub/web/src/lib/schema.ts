/**
 * Turning a tool's JSON Schema into something a person can fill in.
 *
 * Deliberately shallow: only top-level properties of a primitive type get a
 * field. Anything else - nested objects, arrays of objects, oneOf - is left to
 * the raw JSON editor, because a form that half-renders a complex schema is
 * worse than one that admits it cannot.
 */

import type { JsonSchema } from "../api/types";

export type FieldKind = "string" | "number" | "integer" | "boolean" | "enum";

export interface Field {
  name: string;
  kind: FieldKind;
  required: boolean;
  description?: string;
  options?: string[];
  default?: unknown;
}

function kindOf(schema: JsonSchema): FieldKind | null {
  if (Array.isArray(schema.enum) && schema.enum.length > 0) return "enum";

  // A union like ["string", "null"] is treated as its first concrete type:
  // optional-ness is already carried by `required`.
  const raw = Array.isArray(schema.type)
    ? schema.type.find((t) => t !== "null")
    : schema.type;

  switch (raw) {
    case "string":
      return "string";
    case "number":
      return "number";
    case "integer":
      return "integer";
    case "boolean":
      return "boolean";
    default:
      return null;
  }
}

export function fieldsOf(schema: JsonSchema | undefined): Field[] {
  if (!schema || typeof schema !== "object") return [];
  const properties = schema.properties;
  if (!properties || typeof properties !== "object") return [];

  const required = new Set(Array.isArray(schema.required) ? schema.required : []);
  const fields: Field[] = [];

  for (const [name, property] of Object.entries(properties)) {
    if (!property || typeof property !== "object") continue;
    const kind = kindOf(property);
    if (kind === null) continue; // Too complex for a form; JSON mode handles it.

    fields.push({
      name,
      kind,
      required: required.has(name),
      description: typeof property.description === "string" ? property.description : undefined,
      options: kind === "enum" ? (property.enum as unknown[]).map(String) : undefined,
      default: property.default,
    });
  }

  return fields;
}

export interface Collected {
  args: Record<string, unknown>;
  errors: string[];
}

/**
 * Read form values back into arguments.
 *
 * An empty optional field is omitted rather than sent as "", so a tool sees the
 * argument as absent and applies its own default. An empty *required* field is
 * an error, caught here rather than by the server.
 */
export function collect(fields: Field[], values: Record<string, string>): Collected {
  const args: Record<string, unknown> = {};
  const errors: string[] = [];

  for (const field of fields) {
    const raw = values[field.name];

    if (field.kind === "boolean") {
      if (raw === "true" || raw === "false") args[field.name] = raw === "true";
      else if (field.required) errors.push(`${field.name} is required`);
      continue;
    }

    if (raw === undefined || raw === "") {
      if (field.required) errors.push(`${field.name} is required`);
      continue;
    }

    if (field.kind === "number" || field.kind === "integer") {
      const parsed = Number(raw);
      if (Number.isNaN(parsed)) {
        errors.push(`${field.name} must be a number`);
        continue;
      }
      if (field.kind === "integer" && !Number.isInteger(parsed)) {
        errors.push(`${field.name} must be a whole number`);
        continue;
      }
      args[field.name] = parsed;
      continue;
    }

    args[field.name] = raw;
  }

  return { args, errors };
}

/** Seed form values from arguments, for switching back out of JSON mode. */
export function seed(fields: Field[], args: Record<string, unknown>): Record<string, string> {
  const values: Record<string, string> = {};
  for (const field of fields) {
    const value = args[field.name] ?? field.default;
    if (value === undefined || value === null) continue;
    values[field.name] = typeof value === "string" ? value : JSON.stringify(value);
  }
  return values;
}

/** Keys the form cannot represent, so the user can be told what was dropped. */
export function unknownKeys(fields: Field[], args: Record<string, unknown>): string[] {
  const known = new Set(fields.map((f) => f.name));
  return Object.keys(args).filter((key) => !known.has(key));
}
