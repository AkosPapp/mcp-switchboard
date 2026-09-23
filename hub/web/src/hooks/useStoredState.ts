import { useCallback, useState } from "react";

import { readMemory, writeMemory, type Validate } from "../lib/uiMemory";

/** useState that starts from, and writes through to, the browser's memory. */
export function useStoredState<T>(
  key: string,
  fallback: T,
  validate: Validate<T>,
): [T, (next: T) => void] {
  const [value, setValue] = useState<T>(() => readMemory(key, validate, fallback));
  const set = useCallback(
    (next: T) => {
      setValue(next);
      writeMemory(key, next);
    },
    [key],
  );
  return [value, set];
}
