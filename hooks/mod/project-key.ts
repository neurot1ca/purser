export async function projectKeyFromRoot(root: string, os: string | undefined): Promise<string> {
  if (os === 'Windows_NT') root = root.replace(/\//g, '\\').toLowerCase()
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(root))
  return [...new Uint8Array(digest)].map(byte => byte.toString(16).padStart(2, '0')).join('').slice(0, 16)
}
