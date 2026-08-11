<script setup lang="ts">
import { useRouter } from 'vue-router'
import { useSession } from './stores/session'

const session = useSession()
const router = useRouter()

async function signOut() {
  await session.signOut()
  await router.push({ name: 'login' })
}
</script>

<template>
  <header v-if="session.signedIn" class="topbar">
    <span class="brand">KANZ</span>
    <nav>
      <RouterLink to="/estate">Estate</RouterLink>
      <RouterLink to="/nodes">Nodes</RouterLink>
      <RouterLink to="/venues">Venues</RouterLink>
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
