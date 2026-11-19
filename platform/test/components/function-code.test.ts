import { describe, beforeAll, beforeEach, it, expect, vi } from "vitest";
import * as pulumi from "@pulumi/pulumi";
import crypto from "crypto";
import fs from "fs";
import os from "os";
import path from "path";

process.on("unhandledRejection", (err: any) => {
  if (err?.code === "ERR_TRACE_EVENTS_UNAVAILABLE") return;
  throw err;
});

const root = fs.mkdtempSync(path.join(os.tmpdir(), "function-code-"));
const work = path.join(root, ".sst");
const out = path.join(root, "out");
fs.mkdirSync(out);
fs.writeFileSync(path.join(out, "bootstrap"), "a binary\n");

vi.mock("../../src/components/aws/helpers/bootstrap", () => ({
  bootstrap: {
    forRegion: async () => ({ asset: "sst-asset-test" }),
  },
}));

// What the build of each function answers, and what it was asked.
const builds: Record<string, { signature?: string; deployed?: string }> = {};
const asked: Record<string, any> = {};

vi.mock("../../src/components/rpc/rpc", () => ({
  rpc: {
    call: async (method: string, input: any) => {
      if (method === "Runtime.Build") {
        asked[input.functionID] = input;
        const build = builds[input.functionID] ?? {};
        return {
          handler: "bootstrap",
          // a skipped build leaves nothing to zip
          out: build.deployed ? path.join(root, "never-built") : out,
          errors: [],
          sourcemaps: [],
          ...build,
        };
      }
      return {};
    },
  },
}));

// @ts-ignore
global.$app = { name: "app", stage: "test" };
global.$util = pulumi;
// @ts-ignore
global.$dev = false;
// @ts-ignore
global.$cli = {
  paths: { root, work, platform: root },
  state: { version: {} },
};

interface Registered {
  type: string;
  name: string;
  inputs: any;
}

let registered: Registered[] = [];

pulumi.runtime.setMocks(
  {
    newResource: function (args: pulumi.runtime.MockResourceArgs) {
      registered.push({
        type: args.type,
        name: args.name,
        inputs: args.inputs,
      });
      return {
        id: args.id ?? `${args.name}_id`,
        state: { ...args.inputs, arn: `arn:aws:test:::${args.name}` },
      };
    },
    call: function (args: pulumi.runtime.MockCallArgs) {
      if (args.token === "aws:index/getCallerIdentity:getCallerIdentity")
        return { accountId: "111111111111" };
      if (args.token === "aws:index/getPartition:getPartition")
        return { partition: "aws" };
      if (args.token === "aws:index/getRegion:getRegion")
        return { name: "us-east-1", region: "us-east-1" };
      if (args.token === "aws:iam/getPolicyDocument:getPolicyDocument")
        return { ...args.inputs, json: "{}" };
      return args.inputs;
    },
  },
  "project",
  "stack",
  false,
);

// Waits until no resource has been registered for 250 ms.
async function settle() {
  let count = -1;
  let quiet = 0;
  for (let i = 0; i < 400 && quiet < 10; i++) {
    await new Promise((resolve) => setTimeout(resolve, 25));
    quiet = registered.length === count ? quiet + 1 : 0;
    count = registered.length;
  }
}

function resolved<T>(value: pulumi.Input<T>) {
  return new Promise<T>((resolve) =>
    pulumi.output(value).apply((v) => resolve(v as T)),
  );
}

const codeObject = (name: string) =>
  registered.filter(
    (r) =>
      r.type === "aws:s3/bucketObjectv2:BucketObjectv2" &&
      r.name === `${name}Code`,
  );
const lambda = (name: string) =>
  registered.filter(
    (r) =>
      r.type === "aws:lambda/function:Function" && r.name === `${name}Function`,
  );
const zipPath = (name: string) =>
  path.join(work, "artifacts", name, "code.zip");

describe("Function code", function () {
  let Function: typeof import("../../src/components/aws/function").Function;
  let s3: typeof import("@pulumi/aws").s3;
  // options each code object was created with, and outputs each function registered
  let codeOptions: Record<string, pulumi.CustomResourceOptions>;
  let outputs: Record<string, any>;

  beforeAll(async function () {
    Function = (await import("../../src/components/aws/function")).Function;
    s3 = (await import("@pulumi/aws")).s3;
  });

  beforeEach(function () {
    registered = [];
    codeOptions = {};
    outputs = {};
    for (const key of Object.keys(builds)) delete builds[key];
    for (const key of Object.keys(asked)) delete asked[key];
    vi.restoreAllMocks();

    const Original = s3.BucketObjectv2;
    vi.spyOn(s3, "BucketObjectv2").mockImplementation(function (
      name: string,
      args: any,
      opts: any,
    ) {
      codeOptions[name] = opts;
      return new Original(name, args, opts);
    } as any);

    const registerOutputs = (pulumi.ComponentResource.prototype as any)
      .registerOutputs;
    vi.spyOn(
      pulumi.ComponentResource.prototype as any,
      "registerOutputs",
    ).mockImplementation(function (this: any, values: any) {
      if (values && "_code" in values) outputs[this.__name ?? ""] = values;
      return registerOutputs.call(this, values);
    });
  });

  // the outputs of the one function a test creates
  const code = async () => {
    const all = Object.values(outputs);
    expect(all).toHaveLength(1);
    return await resolved<any>(all[0]._code);
  };

  it("builds, zips and records the signature of the build", async () => {
    builds.Built = { signature: "signature-1" };
    new Function("Built", { handler: "services/fn", runtime: "go" });
    await settle();

    // the build was told where the zip goes, so it may be skipped next time
    expect(asked.Built.code).toBe(zipPath("Built"));

    const zip = fs.readFileSync(zipPath("Built"));
    expect(zip.length).toBeGreaterThan(0);
    const hash = crypto.createHash("sha256").update(zip).digest("hex");
    const key = `assets/Built-code-${hash}.zip`;

    expect(codeObject("Built")).toHaveLength(1);
    expect(codeObject("Built")[0].inputs.key).toBe(key);
    expect(codeOptions.BuiltCode.ignoreChanges).toBeUndefined();
    expect(lambda("Built")[0].inputs.s3Key).toBe(key);

    expect(await code()).toEqual({ signature: "signature-1", key });
  });

  it("keeps the deployed zip when the build was skipped", async () => {
    const key = "assets/Kept-code-0123abcd.zip";
    builds.Kept = { signature: "signature-1", deployed: key };
    new Function("Kept", { handler: "services/fn", runtime: "go" });
    await settle();

    // the object keeps its key and the source the state has
    expect(codeObject("Kept")).toHaveLength(1);
    expect(codeObject("Kept")[0].inputs.key).toBe(key);
    expect(codeObject("Kept")[0].inputs.bucket).toBe("sst-asset-test");
    expect(codeOptions.KeptCode.ignoreChanges).toEqual(["source"]);

    // Pulumi is shown an empty file, where the zip would be. Empty, or the
    // provider could later take its copy of it for the real zip.
    expect(fs.readFileSync(zipPath("Kept")).length).toBe(0);
    const source = codeObject("Kept")[0].inputs.source;
    expect(await source.path).toBe(zipPath("Kept"));

    // the function keeps running the same zip, and the same pair is recorded
    expect(lambda("Kept")[0].inputs.s3Key).toBe(key);
    expect(lambda("Kept")[0].inputs.handler).toBe("bootstrap");
    expect(await code()).toEqual({ signature: "signature-1", key });
  });

  it("replaces what a skipped build left when the next one runs", async () => {
    fs.mkdirSync(path.dirname(zipPath("Again")), { recursive: true });
    fs.writeFileSync(zipPath("Again"), "");
    builds.Again = { signature: "signature-2" };
    new Function("Again", { handler: "services/fn", runtime: "go" });
    await settle();

    const zip = fs.readFileSync(zipPath("Again"));
    expect(zip.length).toBeGreaterThan(0);
    const hash = crypto.createHash("sha256").update(zip).digest("hex");
    expect(codeObject("Again")[0].inputs.key).toBe(
      `assets/Again-code-${hash}.zip`,
    );
    expect(codeOptions.AgainCode.ignoreChanges).toBeUndefined();
  });

  it("always builds a function with a postbuild hook", async () => {
    builds.Hooked = { signature: "signature-1" };
    let ran = "";
    new Function("Hooked", {
      handler: "services/fn",
      runtime: "go",
      hook: {
        postbuild: async (dir) => {
          ran = dir;
        },
      },
    });
    await settle();

    // not told where the zip goes: the build cannot be skipped
    expect(asked.Hooked.code).toBeUndefined();
    expect(ran).toBe(out);
    expect(codeOptions.HookedCode.ignoreChanges).toBeUndefined();
  });

  it("records nothing for a build without a signature", async () => {
    new Function("Unsigned", {
      handler: "index.handler",
      runtime: "nodejs22.x",
    });
    await settle();

    expect(codeObject("Unsigned")).toHaveLength(1);
    expect(codeOptions.UnsignedCode.ignoreChanges).toBeUndefined();
    expect(await code()).toBeUndefined();
  });
});
