<script lang="ts">
  import { onMount } from 'svelte';
  import Header, { type TabId } from '$lib/components/layout/Header.svelte';
  import RoomCanvas from '$lib/components/rooms/RoomCanvas.svelte';
  import MeterRack from '$lib/components/meters/MeterRack.svelte';
  import DeviceManager from '$lib/components/devices/DeviceManager.svelte';
  import RoomCoordinator from '$lib/components/rooms/RoomCoordinator.svelte';
  import TelemetryDashboard from '$lib/components/telemetry/TelemetryDashboard.svelte';
  import { authStore } from '$lib/stores/auth.svelte';
  import { meterStore } from '$lib/stores/meters.svelte';
  import { clusterStore } from '$lib/stores/cluster.svelte';
  import { SvelteFlowProvider } from '@xyflow/svelte';
  import AlertCircle from '@lucide/svelte/icons/alert-circle';
  import X from '@lucide/svelte/icons/x';
  import Key from '@lucide/svelte/icons/key';

  let activeTab = $state<TabId>('flow');
  let dismissBanner = $state(false);

  onMount(() => {
    authStore.checkConnection();
    meterStore.init();
    clusterStore.startPolling(2500);
  });
</script>

<div class="flex flex-col h-screen w-screen overflow-hidden bg-studio-dark text-foreground font-sans">
  <!-- Top Navigation Header -->
  <Header bind:activeTab />

  <!-- Connection / Auth Warning Banner -->
  {#if !dismissBanner && (authStore.connectionStatus === 'unauthorized' || authStore.connectionStatus === 'error')}
    <div class="bg-gradient-to-r from-red-950/80 via-zinc-950/90 to-red-950/80 border-b border-red-500/30 px-4 py-2 flex items-center justify-between text-xs text-red-300">
      <div class="flex items-center gap-2">
        <AlertCircle class="w-4 h-4 text-red-400 shrink-0" />
        <span>
          {#if authStore.connectionStatus === 'unauthorized'}
            Authentication required to modify audio crossbar and provision devices.
          {:else}
            {authStore.lastError || 'Unable to connect to audio router daemon at ' + authStore.activeDaemonUrl}. Running in local simulation mode.
          {/if}
        </span>
      </div>
      <div class="flex items-center gap-2">
        <button
          onclick={() => (dismissBanner = true)}
          class="p-1 hover:text-white rounded"
          aria-label="Dismiss warning"
        >
          <X class="w-3.5 h-3.5" />
        </button>
      </div>
    </div>
  {/if}

  <!-- Active View Viewport -->
  <main class="flex-1 overflow-hidden relative">
    {#if activeTab === 'flow'}
      <SvelteFlowProvider>
        <RoomCanvas />
      </SvelteFlowProvider>
    {:else if activeTab === 'meters'}
      <MeterRack />
    {:else if activeTab === 'devices'}
      <DeviceManager />
    {:else if activeTab === 'rooms'}
      <RoomCoordinator />
    {:else if activeTab === 'diagnostics'}
      <TelemetryDashboard />
    {/if}
  </main>
</div>
