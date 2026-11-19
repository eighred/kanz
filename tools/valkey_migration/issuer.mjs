// The qualification parent reads this private pipe in memory. Only the public
// verification key and the revocation feed are served by the disposable issuer.
import { createHash, generateKeyPairSync, sign } from "node:crypto";
import { fstatSync } from "node:fs";

const output = fstatSync(1);
if (!output.isFIFO() && !output.isSocket()) throw new Error("the fixture needs a private output pipe");
const { privateKey, publicKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
const jwk = publicKey.export({ format: "jwk" });
const keyId = createHash("sha256").update(publicKey.export({ type: "spki", format: "der" })).digest("hex");
const now = Math.floor(Date.now() / 1000);
const encode = value => Buffer.from(JSON.stringify(value)).toString("base64url");
function token(tenant, sub) {
  const header = encode({ alg: "ES256", kid: keyId });
  const payload = encode({ iss: "https://identity.kanz.internal", aud: "kanz", sub,
    tenant, roles: ["kanz-user"], session_epoch: "0", iat: now, exp: now + 900 });
  const signed = header + "." + payload;
  return signed + "." + sign("sha256", Buffer.from(signed), { key: privateKey, dsaEncoding: "ieee-p1363" }).toString("base64url");
}

process.stdout.write(JSON.stringify({
  jwks: { keys: [{ ...jwk, alg: "ES256", use: "sig", kid: keyId }] },
  revocations: { kind: "kanz.revocations.v2", as_of: now, entries: [{
    subject_hash: createHash("sha256").update("revoked-fixture-reader").digest("hex"),
    not_before: now, session_epoch: "1",
  }] },
  tokens: { a: token("acme", "fixture-reader"), b: token("other", "fixture-reader"),
    revoked: token("acme", "revoked-fixture-reader") },
}));
