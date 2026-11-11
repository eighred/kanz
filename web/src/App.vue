<script setup lang="ts">
import { workspaces } from './navigation'
import { useRouter } from 'vue-router'
import { useSession } from './stores/session'

const session = useSession()
const router = useRouter()

function closeMenu(event: Event) {
  const menu = (event.target as HTMLElement).closest('details')
  if (menu) {
    menu.open = false
    if (event.type === 'keydown') menu.querySelector('summary')?.focus()
  }
}

async function signOut() {
  await session.signOut()
  await router.push({ name: 'login' })
}
</script>

<template>
  <header v-if="session.signedIn" class="topbar">
    <span class="brand">KANZ</span>
    <nav aria-label="Main navigation">
      <RouterLink to="/overview">Workspace</RouterLink>
      <details v-for="group in workspaces" :key="group.title" name="workspace-navigation" class="nav-group" @keydown.esc="closeMenu">
        <summary>{{ group.title }}</summary>
        <div class="nav-menu">
          <RouterLink v-for="page in group.pages" :key="page.to" :to="page.to" @click="closeMenu">{{ page.title }}</RouterLink>
        </div>
      </details>
    </nav>
    <!-- WHO YOU ARE ACTING AS, on every screen. The most expensive mistake
         available on a multi-tenant control plane is doing the right thing in
         the wrong tenant, and never making the operator guess is the only
         defence the interface itself can offer. -->
    <span class="whoami">
      <strong>{{ session.identity?.subject }}</strong> · {{ session.identity?.tenant }}
    </span>
    <button class="quiet" @click="signOut">Sign out</button>
  </header>
  <main><RouterView /></main>
</template>
