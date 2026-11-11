<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { broker, describeBroker, type BrokerAccount } from '../api/broker'

const accounts = ref<BrokerAccount[]>([])
const loading = ref(true)
const error = ref('')

onMounted(async () => {
  try { accounts.value = await broker.accounts() }
  catch (e) { error.value = describeBroker(e) }
  finally { loading.value = false }
})
</script>

<template>
  <h1>Broker accounts</h1>
  <p class="muted">Read-only account projections folded from recorded orders and executions.</p>
  <p v-if="loading">Loading…</p>
  <p v-else-if="error" class="error" role="alert">{{ error }}</p>
  <div v-else class="workspace-grid">
    <RouterLink v-for="account in accounts" :key="account.id" :to="{ name: 'broker-account', params: { id: account.id } }" class="workspace-card">
      <strong><code>{{ account.id }}</code></strong>
      <span>{{ account.name }} · {{ account.currency }}</span>
    </RouterLink>
    <p v-if="accounts.length === 0">The broker projection contains no accounts for this tenant.</p>
  </div>
</template>
