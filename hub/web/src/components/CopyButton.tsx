import { useToast } from "./Toast";

/**
 * Copy to clipboard, with a fallback.
 *
 * navigator.clipboard is unavailable on a plain-HTTP origin that is not
 * localhost, which is exactly how this console is reached through an SSH
 * tunnel, so the execCommand path is not dead code.
 */
export function copyText(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    return navigator.clipboard.writeText(text);
  }
  return new Promise((resolve, reject) => {
    try {
      const area = document.createElement("textarea");
      area.value = text;
      area.setAttribute("readonly", "");
      area.style.position = "fixed";
      area.style.opacity = "0";
      document.body.appendChild(area);
      area.select();
      document.execCommand("copy");
      document.body.removeChild(area);
      resolve();
    } catch (error) {
      reject(error);
    }
  });
}

interface Props {
  value: string;
  label?: string;
  className?: string;
}

export default function CopyButton({ value, label = "Copy", className = "" }: Props) {
  const toast = useToast();

  return (
    <button
      type="button"
      className={`rounded border border-border px-2 py-1 text-xs text-muted transition-colors hover:bg-raised hover:text-text ${className}`}
      onClick={() => {
        copyText(value).then(
          () => toast.show("copied"),
          () => toast.show("could not copy"),
        );
      }}
    >
      {label}
    </button>
  );
}
