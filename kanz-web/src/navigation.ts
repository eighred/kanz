export const workspaces = [
  { title: 'Governance', pages: [
    { to: '/preflight', title: 'Preflight', description: 'Review verified admission controls and outstanding prerequisites.' },
    { to: '/invitations', title: 'User invitations', description: 'Invite an authorized person to activate their own account.' },
    { to: '/mandate-proposal', title: 'Propose mandate', description: 'Enter approved portfolio terms for independent review.' },
    { to: '/mandate-changes', title: 'Mandate reviews', description: 'Review pending changes and the exact proposed terms.' },
    { to: '/approvals', title: 'Order approvals', description: 'Review orders held for a distinct checker.' },
    { to: '/overrides', title: 'Overrides', description: 'Review data exception override requests.' },
    { to: '/audit', title: 'Audit events', description: 'Search recorded events within your authorized tenant.' },
  ] },
  { title: 'Portfolio and risk', pages: [
    { to: '/portfolios', title: 'Portfolios', description: 'Open portfolio exposure, risk measures, scenarios, and orders.' },
    { to: '/instruments', title: 'Instruments', description: 'Look up supported instrument definitions.' },
  ] },
  { title: 'Operations', pages: [
    { to: '/estate', title: 'Estate', description: 'Read cluster and node health.' },
    { to: '/nodes', title: 'Nodes', description: 'Inspect and manage compute nodes.' },
    { to: '/provisions', title: 'Provisions', description: 'Review infrastructure provisioning.' },
    { to: '/venues', title: 'Venues', description: 'Inspect venue configuration.' },
  ] },
]
