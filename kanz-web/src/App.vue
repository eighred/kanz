<script setup lang="ts">
import { useSession } from './stores/session'
const session = useSession()
</script>

<template>
  <header v-if="session.signedIn">
    <nav>
      <RouterLink to="/venues">Venues</RouterLink>
    </nav>
    <span>{{ session.identity?.subject }} · {{ session.identity?.tenant }}</span>
    <button @click="session.signOut().then(() => $router.push('/login'))">Sign out</button>
  </header>
  <main><RouterView /></main>
</template>
