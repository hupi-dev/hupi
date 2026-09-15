import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// Vitest doesn't auto-run Testing Library's cleanup between tests the way
// Jest's globals do — without this, unmounted components from a previous
// test's render() stick around in the DOM and later tests querying by
// role/text can match duplicates across renders in the same file.
afterEach(() => {
  cleanup();
});
