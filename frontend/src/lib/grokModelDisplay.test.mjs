import assert from "node:assert/strict";
import test from "node:test";
import { DEFAULT_GROK_TEST_MODELS, grokConnectionTestModels, grokDisplayModels } from "./grokModelDisplay.ts";

test("auto model directory follows upstream without modifying whitelist", () => {
 const account={models:[],grok_models:{models:["grok-4.6","grok-4.7"],status:"fresh"}};
 const result=grokDisplayModels(account);
 assert.deepEqual(result,["grok-4.6","grok-4.7"]);
 result.push("new");
 assert.deepEqual(account.models,[]);
 assert.equal(account.grok_models.models.length,2);
});
test("fixed whitelist remains an explicit subset, including a known empty catalog", () => {
 assert.deepEqual(grokDisplayModels({models:["grok-4.6"],grok_models:{models:["grok-4.6","grok-4.7"],status:"fresh"}}),["grok-4.6"]);
 assert.deepEqual(grokDisplayModels({models:["grok-4.6"],grok_models:{models:[],status:"fresh"}}),[]);
 assert.deepEqual(grokDisplayModels({models:["grok-4.6"]}),["grok-4.6"]);
});
test("connection test models follow the catalog, drop image models and only default when the catalog is unknown", () => {
 assert.deepEqual(grokConnectionTestModels({models:[],grok_models:{models:["grok-4.7","grok-imagine-image"],status:"fresh"}}),["grok-4.7"]);
 assert.deepEqual(grokConnectionTestModels({models:[],grok_models:{models:[],status:"fresh"}}),[]);
 assert.deepEqual(grokConnectionTestModels({models:[]}),DEFAULT_GROK_TEST_MODELS);
 assert.deepEqual(grokConnectionTestModels({models:[],grok_models:{models:[],status:"unknown"}}),DEFAULT_GROK_TEST_MODELS);
});
