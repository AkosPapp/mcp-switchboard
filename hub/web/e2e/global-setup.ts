import { start } from "./fixtures";

/** One hub and one client for the whole run; tearing them down between tests
 * would cost more than the tests themselves. */
export default async function globalSetup() {
  await start();
}
