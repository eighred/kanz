# Invitation email domains

Set `IDENTITY_INVITE_EMAIL_DOMAINS` to a comma-separated list of exact domains,
for example `eighred.co` for company-only invitations or `eighred.co,gmail.com`
to permit both. Restart the identity service after changing its configuration.
The bootstrap `kanz-invite` command accepts the same environment variable; supply
the deployment policy when running that command.

An empty value preserves unrestricted legacy subjects. A configured policy accepts
bare email subjects and `user:<email>` subjects. Domains are case-insensitive.
Subdomains must be listed separately. Wildcards, top-level suffixes such as `com`,
malformed domains, and empty list entries fail configuration loading.

The authenticated invitation endpoint checks the policy before minting or storing
an invitation. The policy applies to new invitations; it does not revoke existing
invitations, disable accounts, change login identifiers, or verify mailbox ownership.
An invitation remains bound to its original subject and authority at redemption.
Different email addresses do not establish that two independent people hold them.

Disable unwanted existing accounts through the authenticated
`POST /users/{subject}/disable` endpoint. Retain identity rows so the revocation
feed continues rejecting outstanding tokens and audit records remain attributable.
