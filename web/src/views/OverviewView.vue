<script setup lang="ts">
import { workspaces } from '../navigation'
</script>

<template>
  <h1>Workspace</h1>
  <p class="muted">Choose a task. Each page checks access with the server when opened.</p>
  <p class="workspace-notice">Before any trading decision, review the latest <RouterLink to="/preflight">preflight evidence</RouterLink> and required human approvals. This directory does not establish trading readiness.</p>
  <section v-for="group in workspaces" :key="group.title" class="workspace-group">
    <h2>{{ group.title }}</h2>
    <div class="workspace-grid">
      <RouterLink v-for="page in group.pages" :key="page.to" :to="page.to" class="workspace-card">
        <strong>{{ page.title }}</strong>
        <span>{{ page.description }}</span>
      </RouterLink>
    </div>
  </section>
</template>
