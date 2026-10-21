<script lang="ts">
  import { authStore } from '$lib/stores/auth.svelte';
  import X from '@lucide/svelte/icons/x';
  import Key from '@lucide/svelte/icons/key';
  import Server from '@lucide/svelte/icons/server';
  import Check from '@lucide/svelte/icons/check';
  import AlertCircle from '@lucide/svelte/icons/alert-circle';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';

  let { isOpen = $bindable(false) } = $props<{ isOpen: boolean }>();

  let inputUrl = $state(authStore.activeDaemonUrl);
  let inputToken = $state(authStore.authToken);
  let isTesting = $state(false);
  let testResult = $state<'success' | 'error' | null>(null);

  // Sync inputs when opened
  $effect(() => {
    if (isOpen) {
      inputUrl = authStore.activeDaemonUrl;
      inputToken = authStore.authToken;
      testResult = null;
    }
  });

  async function testConnection() {
    isTesting = true;
    testResult = null;
    authStore.setDaemonUrl(inputUrl);
    authStore.setToken(inputToken);
    const ok = await authStore.checkConnection();
    testResult = ok ? 'success' : 'error';
    isTesting = false;
  }

  function handleSave() {
    authStore.setDaemonUrl(inputUrl);
    authStore.setToken(inputToken);
    authStore.checkConnection();
    isOpen = false;
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      isOpen = false;
    }
  }
</script>

<svelte:window onkeydown={handleKeydown} />

{#if isOpen}
  <!-- Backdrop -->
  <div
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/80 backdrop-blur-sm p-4 transition-all"
    role="dialog"
    aria-modal="true"
    aria-labelledby="auth-modal-title"
  >
    <!-- Modal Card -->
    <div
      class="w-full max-w-md rounded-xl border border-zinc-800 bg-zinc-950 p-6 shadow-2xl shadow-cyan-950/20 text-zinc-100"
    >
      <!-- Header -->
      <div class="flex items-center justify-between border-b border-zinc-800/80 pb-4">
        <div class="flex items-center gap-2.5">
          <div class="flex h-8 w-8 items-center justify-center rounded-lg bg-cyan-500/10 border border-cyan-500/20 text-cyan-400">
            <Key class="w-4 h-4" />
          </div>
          <div>
            <h2 id="auth-modal-title" class="text-base font-semibold tracking-wide text-zinc-100">
              Gateway & Security
            </h2>
            <p class="text-xs text-zinc-400">Configure control plane daemon connection</p>
          </div>
        </div>
        <button
          onclick={() => (isOpen = false)}
          class="rounded-lg p-1.5 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-200 transition-colors"
          aria-label="Close dialog"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <!-- Body -->
      <div class="space-y-4 pt-5 pb-4">
        <!-- Daemon URL -->
        <div>
          <label for="daemon-url" class="block text-xs font-mono font-medium text-zinc-300 mb-1.5">
            DAEMON GATEWAY URL
          </label>
          <div class="relative">
            <span class="absolute inset-y-0 left-0 flex items-center pl-3 pointer-events-none text-zinc-500">
              <Server class="w-4 h-4" />
            </span>
            <input
              id="daemon-url"
              type="text"
              bind:value={inputUrl}
              placeholder="http://localhost:8000"
              class="w-full rounded-lg border border-zinc-800 bg-zinc-900/90 pl-9 pr-3 py-2 text-sm font-mono text-zinc-100 placeholder-zinc-600 focus:border-cyan-500 focus:outline-none focus:ring-1 focus:ring-cyan-500"
            />
          </div>
          <p class="mt-1 text-[11px] text-zinc-500">Default local daemon runs on port 8000</p>
        </div>

        <!-- Token -->
        <div>
          <label for="auth-token" class="block text-xs font-mono font-medium text-zinc-300 mb-1.5">
            AUTHENTICATION TOKEN (BEARER)
          </label>
          <div class="relative">
            <span class="absolute inset-y-0 left-0 flex items-center pl-3 pointer-events-none text-zinc-500">
              <Key class="w-4 h-4" />
            </span>
            <input
              id="auth-token"
              type="password"
              bind:value={inputToken}
              placeholder="e.g. sec_token_..."
              class="w-full rounded-lg border border-zinc-800 bg-zinc-900/90 pl-9 pr-3 py-2 text-sm font-mono text-zinc-100 placeholder-zinc-600 focus:border-cyan-500 focus:outline-none focus:ring-1 focus:ring-cyan-500"
            />
          </div>
          <p class="mt-1 text-[11px] text-zinc-500">Passed via Authorization: Bearer & X-Auth-Token</p>
        </div>

        <!-- Test Connection Feedback -->
        {#if testResult === 'success'}
          <div class="flex items-center gap-2 rounded-lg border border-emerald-500/30 bg-emerald-950/30 px-3 py-2 text-xs text-emerald-400">
            <Check class="w-4 h-4 shrink-0" />
            <span>Connection verified (Audio Router v{authStore.version})</span>
          </div>
        {:else if testResult === 'error'}
          <div class="flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-950/30 px-3 py-2 text-xs text-red-400">
            <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
            <span>{authStore.lastError || 'Failed to connect to gateway daemon'}</span>
          </div>
        {/if}
      </div>

      <!-- Footer Buttons -->
      <div class="flex items-center justify-between border-t border-zinc-800/80 pt-4">
        <button
          type="button"
          onclick={testConnection}
          disabled={isTesting}
          class="inline-flex items-center gap-1.5 rounded-lg border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-xs font-medium text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100 transition-colors disabled:opacity-50"
        >
          <RefreshCw class="w-3.5 h-3.5 {isTesting ? 'animate-spin text-cyan-400' : ''}" />
          <span>{isTesting ? 'Testing...' : 'Test Ping'}</span>
        </button>

        <div class="flex items-center gap-2">
          <button
            type="button"
            onclick={() => (isOpen = false)}
            class="rounded-lg px-3 py-1.5 text-xs font-medium text-zinc-400 hover:text-zinc-200 transition-colors"
          >
            Cancel
          </button>
          <button
            type="button"
            onclick={handleSave}
            class="inline-flex items-center gap-1.5 rounded-lg bg-cyan-500 px-4 py-1.5 text-xs font-semibold text-zinc-950 shadow-sm shadow-cyan-500/30 hover:bg-cyan-400 transition-colors"
          >
            Save & Connect
          </button>
        </div>
      </div>
    </div>
  </div>
{/if}
