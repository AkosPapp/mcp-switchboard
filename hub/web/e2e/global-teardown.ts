import { stop } from "./fixtures";

export default async function globalTeardown() {
  stop();
}
