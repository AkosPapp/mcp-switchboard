import type { ApprovalMode } from "../../api/types";
import type { Profile, ProfileInput } from "./api";

export interface FormValues {
  name: string;
  description: string;
  systemPrompt: string;
  /** "" = hub default, otherwise "provider/model". */
  modelKey: string;
  canSpawn: boolean;
  canMessage: boolean;
  approval: ApprovalMode;
  budget: string;
}

export const APPROVALS: { value: ApprovalMode; help: string }[] = [
  { value: "never", help: "Tools run without asking." },
  { value: "destructive", help: "Ask before tools marked destructive; the rest run freely." },
  { value: "always", help: "Ask before every tool call." },
];

export function modelKeyOf(profile: Profile | null): string {
  return profile?.model ? `${profile.model.provider}/${profile.model.model}` : "";
}

export function initialValues(profile: Profile | null): FormValues {
  return {
    name: profile?.name ?? "",
    description: profile?.description ?? "",
    systemPrompt: profile?.systemPrompt ?? "",
    modelKey: modelKeyOf(profile),
    canSpawn: profile?.capabilities.canSpawn ?? false,
    canMessage: profile?.capabilities.canMessage ?? false,
    approval: profile?.approval ?? "destructive",
    budget: JSON.stringify(profile?.budget ?? {}, null, 2),
  };
}

export type FormErrors = Partial<Record<"name" | "systemPrompt" | "budget", string>>;

/** Validate and turn the form into the request body, or return the errors. */
export function toInput(
  values: FormValues,
  original: Profile | null,
): { input: ProfileInput; errors?: undefined } | { errors: FormErrors; input?: undefined } {
  const errors: FormErrors = {};
  if (values.name.trim() === "") errors.name = "Name is required";
  if (values.systemPrompt.trim() === "") errors.systemPrompt = "System prompt is required";

  let budget: Record<string, number> = {};
  try {
    const parsed = JSON.parse(values.budget.trim() || "{}");
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      errors.budget = "Budget must be a JSON object";
    } else if (Object.values(parsed).some((v) => typeof v !== "number")) {
      errors.budget = "Budget values must be numbers";
    } else {
      budget = parsed as Record<string, number>;
    }
  } catch {
    errors.budget = "Budget is not valid JSON";
  }
  if (Object.keys(errors).length > 0) return { errors };

  let model: Profile["model"] = null;
  if (values.modelKey !== "") {
    if (values.modelKey === modelKeyOf(original)) {
      model = original!.model; // keep any extra keys the hub stored
    } else {
      const [provider, ...rest] = values.modelKey.split("/");
      model = { provider, model: rest.join("/") };
    }
  }
  return {
    input: {
      name: values.name.trim(),
      description: values.description,
      systemPrompt: values.systemPrompt,
      model,
      capabilities: { canSpawn: values.canSpawn, canMessage: values.canMessage },
      approval: values.approval,
      budget,
    },
  };
}
