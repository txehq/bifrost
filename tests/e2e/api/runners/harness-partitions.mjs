#!/usr/bin/env node
// Prints the harness sweep's roster of named partitions, space-separated, for the Makefile's
// HARNESS_PROVIDERS (fork, TXE-7308): every registered provider in schemas.StandardProviders,
// then the reseller custom providers and the route families of lib/harness-partitions.mjs.
//
//   node tests/e2e/api/runners/harness-partitions.mjs
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { harnessPartitions } from "./lib/harness-partitions.mjs";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "..", "..");
process.stdout.write(`${harnessPartitions(repoRoot).join(" ")}\n`);
