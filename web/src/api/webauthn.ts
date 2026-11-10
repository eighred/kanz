export interface MFACeremony {
  id: string
  options: { publicKey: Record<string, any> }
}
export class WebAuthnError extends Error {
  constructor() { super('Security-key verification was not completed. Try again with an enrolled key or passkey.'); this.name = 'WebAuthnError' }
}
function decode(value: string): ArrayBuffer {
  const raw = atob(value.replace(/-/g, '+').replace(/_/g, '/'))
  return Uint8Array.from(raw, c => c.charCodeAt(0)).buffer
}
function encode(value: ArrayBuffer): string {
  return btoa(String.fromCharCode(...new Uint8Array(value))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}
function descriptors(values: any[] = []): PublicKeyCredentialDescriptor[] {
  return values.map(v => ({ ...v, id: decode(v.id) }))
}
export async function webAuthn(c: MFACeremony, registration: boolean, signal?: AbortSignal): Promise<unknown> {
  try {
    if (!navigator.credentials || !window.PublicKeyCredential) throw new WebAuthnError()
    const p = c.options.publicKey
    let result: Credential | null
    if (registration) {
      const publicKey = { ...p, challenge: decode(p.challenge), user: { ...p.user, id: decode(p.user.id) }, excludeCredentials: descriptors(p.excludeCredentials) } as PublicKeyCredentialCreationOptions
      result = await navigator.credentials.create({ publicKey, signal })
    } else {
      const publicKey = { ...p, challenge: decode(p.challenge), allowCredentials: descriptors(p.allowCredentials) } as PublicKeyCredentialRequestOptions
      result = await navigator.credentials.get({ publicKey, signal })
    }
    if (!result || signal?.aborted) throw new WebAuthnError()
    const key = result as PublicKeyCredential
    const response = key.response
    const common = { id: key.id, rawId: encode(key.rawId), type: key.type, clientExtensionResults: key.getClientExtensionResults() }
    if (registration) {
      const r = response as AuthenticatorAttestationResponse
      return { ...common, response: { clientDataJSON: encode(r.clientDataJSON), attestationObject: encode(r.attestationObject), transports: r.getTransports?.() ?? [] } }
    }
    const r = response as AuthenticatorAssertionResponse
    return { ...common, response: { clientDataJSON: encode(r.clientDataJSON), authenticatorData: encode(r.authenticatorData), signature: encode(r.signature), userHandle: r.userHandle ? encode(r.userHandle) : null } }
  } catch { throw new WebAuthnError() }
}
