<script lang="ts">
  import { authStore } from '$lib/stores/auth.svelte';
  import { peerStore } from '$lib/stores/peers.svelte';
  import { clusterStore } from '$lib/stores/cluster.svelte';
  import { matrixStore } from '$lib/stores/matrix.svelte';
  import AuthModal from './AuthModal.svelte';
  import Radio from '@lucide/svelte/icons/radio';
  import Sliders from '@lucide/svelte/icons/sliders';
  import Activity from '@lucide/svelte/icons/activity';
  import Speaker from '@lucide/svelte/icons/speaker';
  import Network from '@lucide/svelte/icons/network';
  import Cpu from '@lucide/svelte/icons/cpu';
  import Shield from '@lucide/svelte/icons/shield';
  import Key from '@lucide/svelte/icons/key';
  import Clock from '@lucide/svelte/icons/clock';
  import Workflow from '@lucide/svelte/icons/workflow';

  export type TabId = 'flow' | 'meters' | 'devices' | 'rooms' | 'diagnostics';

  let { activeTab = $bindable('flow' as TabId) } = $props<{ activeTab: TabId }>();

  let isAuthOpen = $state(false);

  const tabs: { id: TabId; label: string; icon: any }[] = [
    { id: 'flow', label: 'Audio Flow', icon: Workflow },
    { id: 'meters', label: 'Live Meters', icon: Activity },
    { id: 'devices', label: 'Audio Devices', icon: Speaker },
    { id: 'rooms', label: 'Mesh Rooms', icon: Network },
    { id: 'diagnostics', label: 'Cluster Diagnostics', icon: Cpu },
  ];

  function getStatusBadge(status: string) {
    switch (status) {
      case 'connected':
        return { color: 'bg-emerald-500', glow: 'shadow-[0_0_8px_rgba(16,185,129,0.8)]', label: 'ONLINE' };
      case 'connecting':
        return { color: 'bg-amber-500 animate-pulse', glow: 'shadow-[0_0_8px_rgba(245,158,11,0.8)]', label: 'SYNCING' };
      case 'unauthorized':
        return { color: 'bg-purple-500', glow: 'shadow-[0_0_8px_rgba(168,85,247,0.8)]', label: 'AUTH REQ' };
      default:
        return { color: 'bg-red-500', glow: 'shadow-[0_0_8px_rgba(239,68,68,0.8)]', label: 'OFFLINE' };
    }
  }

  const badge = $derived(getStatusBadge(authStore.connectionStatus));

  const clusterHealthDotColor = $derived.by(() => {
    const score = clusterStore.clusterHealthScore;
    if (score >= 90) return 'bg-emerald-400 shadow-[0_0_8px_rgba(52,211,153,0.8)]';
    if (score >= 75) return 'bg-cyan-400 shadow-[0_0_8px_rgba(34,211,238,0.8)]';
    if (score >= 50) return 'bg-amber-400 shadow-[0_0_8px_rgba(251,191,36,0.8)]';
    return 'bg-red-400 shadow-[0_0_8px_rgba(248,113,113,0.8)]';
  });

  const clusterHealthTextColor = $derived.by(() => {
    const score = clusterStore.clusterHealthScore;
    if (score >= 90) return 'text-emerald-400';
    if (score >= 75) return 'text-cyan-400';
    if (score >= 50) return 'text-amber-400';
    return 'text-red-400';
  });
</script>

<header class="h-14 border-b border-zinc-800 bg-zinc-950/95 backdrop-blur px-4 flex items-center justify-between select-none shrink-0 z-40">
  <!-- Brand & Node Info -->
  <div class="flex items-center gap-4">
    <div class="flex items-center gap-2.5">
      <div class="flex h-8 w-8 items-center justify-center rounded-lg bg-cyan-500/10 border border-cyan-500/30 text-cyan-400 shadow-sm shadow-cyan-500/20">
        <Radio class="w-4 h-4 animate-pulse" />
      </div>
      <div>
        <div class="flex items-center gap-1.5 leading-none">
          <span class="font-mono font-bold tracking-wider text-xs text-zinc-100">AUDIO-ROUTER</span>
          <span class="rounded bg-cyan-500/20 px-1 py-0.5 text-[9px] font-mono font-semibold tracking-wide text-cyan-400 border border-cyan-500/30">
            P2P
          </span>
        </div>
        <span class="text-[10px] font-mono text-zinc-400 tracking-tight">CONTROL PLANE v{authStore.version}</span>
      </div>
    </div>

    <!-- Daemon URL Switcher / Pill -->
    <button
      onclick={() => (isAuthOpen = true)}
      class="group flex items-center gap-2 rounded-lg border border-zinc-800 bg-zinc-900/80 px-2.5 py-1 text-xs hover:border-zinc-700 hover:bg-zinc-800/80 transition-all"
      title="Click to configure Daemon Gateway connection"
    >
      <span class="h-2 w-2 rounded-full {badge.color} {badge.glow}"></span>
      <span class="font-mono text-xs text-zinc-300 group-hover:text-cyan-300 truncate max-w-[170px]">
        {authStore.activeDaemonUrl.replace(/^https?:\/\//, '')}
      </span>
      <span class="text-[10px] font-mono text-zinc-400">({badge.label})</span>
    </button>
  </div>

  <!-- Navigation Tabs -->
  <nav class="flex items-center rounded-lg border border-zinc-800/80 bg-zinc-900/60 p-1" aria-label="Main Navigation">
    {#each tabs as tab}
      {@const IconComponent = tab.icon}
      <button
        onclick={() => (activeTab = tab.id)}
        class="flex items-center gap-1.5 rounded-md px-3 py-1 text-xs font-medium transition-all {activeTab === tab.id
          ? 'bg-cyan-500 text-zinc-950 shadow-sm shadow-cyan-500/30 font-semibold'
          : 'text-zinc-400 hover:text-zinc-200 hover:bg-zinc-800/50'}"
      >
        <IconComponent class="w-3.5 h-3.5" />
        <span>{tab.label}</span>
      </button>
    {/each}
  </nav>

  <!-- Status Badges & Auth Settings Action -->
  <div class="flex items-center gap-3">
    <!-- Cluster Observability Status Badge (ADR 0011) -->
    <button
      onclick={() => (activeTab = 'diagnostics')}
      class="flex items-center gap-2 rounded-lg border border-zinc-800 bg-zinc-900/80 px-2.5 py-1 text-xs hover:border-cyan-500/50 hover:bg-zinc-800/80 transition-all font-mono"
      title="Cluster Health: Click to open Cluster Observability Surface"
    >
      <span class="h-2 w-2 rounded-full {clusterHealthDotColor}"></span>
      <span class="text-zinc-200 font-semibold">{clusterStore.nodes.length || 1} {clusterStore.nodes.length === 1 ? 'Node' : 'Nodes'}</span>
      <span class="text-zinc-500 hidden sm:inline">&bull;</span>
      <span class="{clusterHealthTextColor} font-bold hidden sm:inline">
        {clusterStore.clusterHealthScore.toFixed(0)}% Health
      </span>
    </button>

    <!-- PTP Sync Indicator -->
    <div
      class="hidden md:flex items-center gap-1.5 rounded-md border border-zinc-800/80 bg-zinc-900/50 px-2 py-1 text-[11px] font-mono text-zinc-300"
      title="Distributed PTP Clock Sync"
    >
      <Clock class="w-3 h-3 text-cyan-400" />
      <span class="text-zinc-400">PTP:</span>
      <span class="text-emerald-400 font-semibold">
        {peerStore.clockSync.locked ? 'LOCKED' : 'UNSYNC'}
      </span>
      <span class="text-zinc-400">
        ({peerStore.clockSync.drift_ppm.toFixed(1)} ppm)
      </span>
    </div>

    <!-- Active Peers Count -->
    <div
      class="hidden sm:flex items-center gap-1.5 rounded-md border border-zinc-800/80 bg-zinc-900/50 px-2 py-1 text-[11px] font-mono text-zinc-300"
      title="Discovered Mesh Nodes"
    >
      <Network class="w-3 h-3 text-cyan-400" />
      <span class="text-zinc-400">Peers:</span>
      <span class="text-cyan-300 font-semibold">{peerStore.peers.length}</span>
    </div>

    <!-- Security Dialog Trigger -->
    <button
      onclick={() => (isAuthOpen = true)}
      class="flex items-center gap-1.5 rounded-lg border border-zinc-800 bg-zinc-900 px-2.5 py-1.5 text-xs text-zinc-300 hover:border-cyan-500/50 hover:text-cyan-300 transition-colors"
      aria-label="Open security and connection settings"
      title="Daemon Security Settings"
    >
      <Key class="w-3.5 h-3.5 text-cyan-400" />
      <span class="hidden lg:inline text-xs font-mono">Gateway</span>
    </button>
  </div>
</header>

<AuthModal bind:isOpen={isAuthOpen} />
