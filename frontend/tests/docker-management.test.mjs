import test from "node:test";
import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import ts from "typescript";
const js=ts.transpileModule(readFileSync(new URL("../src/lib/docker-management.ts",import.meta.url),"utf8"),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText;
const {dockerImages,registryCredential,dockerPullInput,dockerCapability}=await import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);
const image=`registry.example/team/app@sha256:${"a".repeat(64)}`;
const credential={id:"opaque",name:"prod",registry:"registry.example",username:"robot",revision:7,credential_configured:true};
test("image inventory requires authoritative exact digest; tags and daemon IDs cannot become targets",()=>{
 assert.deepEqual(dockerImages({items:[{image,id:"sha256:daemon",size_bytes:42,RepoTags:["app:latest"]},{id:"sha256:daemon",RepoTags:["app:latest"],size_bytes:42},{image:"app:latest",id:"daemon",size_bytes:42}]}),[{image,id:"sha256:daemon",size_bytes:42}]);
 assert.throws(()=>dockerPullInput("docker-1","app:latest"));
});
test("credential projection drops returned secrets and pull pins only opaque credential version",()=>{
 assert.deepEqual(registryCredential({...credential,password:"never-display",secret_ref:"never-display"}),credential);
 assert.deepEqual(dockerPullInput("docker-1",image,credential),{id:"docker-1",image,credential_id:"opaque",credential_revision:7});
 assert.deepEqual(dockerPullInput("docker-1",image),{id:"docker-1",image});
 for(const revision of [0,-1,1.5,undefined]) assert.throws(()=>registryCredential({...credential,revision}));
});
test("schema gate rejects task passwords, missing scopes and tag-only image contracts",()=>{
 const c={name:"docker_registry_credential_create",scope:"containers:manage",input_schema:{properties:{password:{format:"password"}}}};
 assert.ok(dockerCapability([c],c.name,["password"],false));
 assert.equal(dockerCapability([{...c,task:true}],c.name,["password"],false),undefined);
 assert.equal(dockerCapability([{...c,scope:"plugins:manage"}],c.name,["password"]),undefined);
 const pull={name:"docker_image_pull",scope:"containers:manage",task:true,input_schema:{properties:{id:{},image:{type:"string"}}}};
 assert.equal(dockerCapability([pull],pull.name,["id","image"],true),undefined);
 assert.ok(dockerCapability([{...pull,input_schema:{properties:{id:{},image:{pattern:"@sha256:[a-f0-9]{64}"}}}}],pull.name,["id","image"],true));
});
