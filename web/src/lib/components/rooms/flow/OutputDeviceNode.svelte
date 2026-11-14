<script lang="ts">
  import { Handle, Position } from '@xyflow/svelte';
  import type { AudioDevice, DeviceHealth, RouteHealth } from '$lib/api/types';
  import { meterStore } from '$lib/stores/meters.svelte';
  import { telemetryStore } from '$lib/stores/telemetry.svelte';
  import { urnChecksum } from '$lib/utils/checksum';
  import {
    breakdownLines,
    devicePillSeverity,
    isTelemetryFresh,
    routeTouchesDevice,
    type DeviceIdentity,
  } from '$lib/utils/artifacts';
  import HealthPill from './HealthPill.svelte';
  import Speaker from '@lucide/svelte/icons/speaker';
  import Headphones from '@lucide/svelte/icons/headphones';
  import Plus from '@lucide/svelte/icons/plus';
  import X from '@lucide/svelte/icons/x';
  import Layers from '@lucide/svelte/icons/layers';
  import Volume2 from '@lucide/svelte/icons/volume-2';
  import VolumeX from '@lucide/svelte/icons/volume-x';
  import Trash2 from '@lucide/svelte/icons/trash-2';

  interface RoomSummary {
    id: string;
    name: string;
  }

  interface OutputNodeData {
    device: AudioDevice;
    sourceRooms: RoomSummary[];
    allRooms: RoomSummary[];
    onAddToRoom: (roomId: string) => void;
    onRemoveFromRoom: (roomId: string) => void;
    onDismiss?: () => void;
    onVolumeChange?: (vol: number) => void;
    onToggleMute?: () => void;
    onDeleteVirtual?: () => void;
    onInspectRoute?: () => void;
  }

  let { id, data, selected = false }: { id: string; data: OutputNodeData; selected?: boolean } = $props();

  let showAddMenu = $state(false);

  const availableRooms = $derived(
    data.allRooms.filter((r) => !data.sourceRooms.some((source) => source.id === r.id))
  );

  const deviceHash = $derived.by(() => {
    const raw = data.device.urn || data.device.id || '';
    return urnChecksum(raw);
  });

  const deviceTail = $derived.by(() => {
    const raw = data.device.urn || data.device.id || '';
    if (raw.includes(':')) {
      const parts = raw.split(':');
      return parts[parts.length - 1];
    }
    return '';
  });

  // Meter levels from meterStore, dynamically resolved to the most recent active stream
  const deviceStream = $derived.by(() => {
    const now = Date.now();
    const candidates = [];

    for (const [sId, st] of Object.entries(meterStore.streams)) {
      // Must not be stale (frames received within last 1500ms or demo simulation)
      if (st.streamId !== 'demo-multitrack-bus' && now - st.lastUpdated > 1500) {
        continue;
      }

      const matchesDirect =
        sId.includes(data.device.id) ||
        (data.device.urn && sId.includes(data.device.urn)) ||
        (deviceHash && sId.includes(deviceHash)) ||
        (deviceTail && sId.includes(deviceTail)) ||
        (data.device.legacy_id && sId.includes(data.device.legacy_id));

      const matchesRoom = data.sourceRooms.some((r) => {
        if (!sId.includes(r.id)) return false;
        // If room matches, verify it is for this output endpoint or room route
        return (
          matchesDirect ||
          (deviceHash && sId.includes(deviceHash)) ||
          sId.startsWith(`room-${r.id}`)
        );
      });

      if (matchesDirect || matchesRoom) {
        candidates.push(st);
      }
    }

    if (candidates.length === 0) return null;
    // Deterministic selection: prioritize explicit :out streams, then most recently updated stream
    candidates.sort((a, b) => {
      const aIsOut = a.streamId.endsWith(':out') ? 1 : 0;
      const bIsOut = b.streamId.endsWith(':out') ? 1 : 0;
      if (aIsOut !== bIsOut) {
        return bIsOut - aIsOut;
      }
      return b.lastUpdated - a.lastUpdated;
    });
    return candidates[0];
  });

  const leftMeter = $derived(
    deviceStream?.channels[0] ??
      (meterStore.useSimulation && data.sourceRooms.length > 0 ? meterStore.activeChannelMeters[0] : null) ??
      { rmsDb: -96, peakDb: -96, clipped: false }
  );
  const rightMeter = $derived(
    deviceStream?.channels[1] ??
      (meterStore.useSimulation && data.sourceRooms.length > 0 ? meterStore.activeChannelMeters[1] : null) ??
      { rmsDb: -96, peakDb: -96, clipped: false }
  );

  function dbToPercent(db: number): number {
    if (db <= -60) return 0;
    if (db >= 0) return 100;
    return Math.round(((db + 60) / 60) * 100);
  }

  const leftPeakPercent = $derived(dbToPercent(leftMeter.peakDb));
  const rightPeakPercent = $derived(dbToPercent(rightMeter.peakDb));

  // ==========================================================================
  // ADR 0034: per-connection audio artifact HealthPill
  // Pill = WORST of the device's own severity (URN-keyed device-artifacts)
  // and the severities of all routes touching this device. Rendered only
  // when FRESH artifact data exists — "no data" (aggregator unwired) or
  // stale telemetry (> 1500 ms, the meter staleness convention) renders
  // nothing: a missing pill must never look like a fake-clean one.
  // ==========================================================================
  const deviceIdentity = $derived<DeviceIdentity>({
    urn: data.device.urn,
    id: data.device.id,
    legacyId: data.device.legacy_id,
    checksum: deviceHash,
    tail: deviceTail,
  });

  const deviceArtifact = $derived.by<DeviceHealth | null>(() => {
    const keys = [data.device.urn, data.device.id].filter((k): k is string => !!k);
    for (const k of keys) {
      const hit = telemetryStore.deviceArtifacts[k];
      if (hit) return hit;
    }
    return null;
  });

  const touchingRouteHealth = $derived.by<Array<{ routeId: string; health: RouteHealth }>>(() => {
    const out: Array<{ routeId: string; health: RouteHealth }> = [];
    for (const [routeId, st] of Object.entries(telemetryStore.streamStats)) {
      if (st.health && routeTouchesDevice(routeId, st.devices, deviceIdentity)) {
        out.push({ routeId, health: st.health });
      }
    }
    return out;
  });

  const pillSeverity = $derived.by<string | null>(() => {
    if (!isTelemetryFresh(telemetryStore.lastUpdated, Date.now())) return null;
    return devicePillSeverity(
      deviceArtifact?.severity ?? null,
      touchingRouteHealth.map((r) => r.health.severity)
    );
  });

  const pillLines = $derived(
    pillSeverity === null
      ? []
      : breakdownLines(
          { urn: data.device.urn || data.device.id, health: deviceArtifact },
          touchingRouteHealth
        )
  );
</script>

<div
  class="relative w-80 rounded-xl border bg-zinc-900/95 backdrop-blur-md p-3.5 shadow-2xl transition-all select-none {selected
    ? 'border-purple-400 ring-2 ring-purple-500/30 shadow-purple-500/20'
    : data.device.status === 'quarantined'
      ? 'border-amber-500/60 ring-1 ring-amber-500/30'
      : 'border-zinc-800 hover:border-zinc-700 shadow-black/60'}"
>
  <!-- Svelte Flow Target Handle (Audio flows from Room to Output) -->
  <Handle
    type="target"
    position={Position.Left}
    id="in"
    class="!w-3.5 !h-3.5 !bg-purple-400 !border-2 !border-zinc-950 !rounded-full shadow-[0_0_8px_rgba(168,85,247,0.8)] hover:shadow-[0_0_14px_rgba(168,85,247,1)] hover:brightness-125 hover:ring-2 hover:ring-purple-300/80 transition-all cursor-crosshair"
    title="Drop wire here to feed room audio into this output"
  />

  <!-- Header -->
  <div class="flex items-start justify-between gap-2">
    <div class="flex items-center gap-2 min-w-0 flex-1">
      <div class="flex h-7 w-7 items-center justify-center rounded-lg bg-purple-500/10 border border-purple-500/30 text-purple-400 shrink-0">
        {#if data.device.is_virtual}
          <Headphones class="w-3.5 h-3.5 text-purple-300" />
        {:else}
          <Speaker class="w-3.5 h-3.5 text-purple-400" />
        {/if}
      </div>
      <div class="min-w-0 flex-1">
        <h3 class="text-xs font-bold text-zinc-100 truncate" title={data.device.name}>
          {data.device.name}
        </h3>
        <span class="text-[10px] font-mono text-zinc-400 truncate block">
          {data.device.id}
        </span>
      </div>
    </div>

    <!-- Direction Badges & Dismiss -->
    <div class="flex items-center gap-1 shrink-0">
      {#if pillSeverity !== null}
        <HealthPill severity={pillSeverity} lines={pillLines} />
      {/if}
      {#if data.device.status === 'quarantined'}
        <span class="rounded bg-amber-500/20 px-1.5 py-0.5 text-[9px] font-mono font-bold uppercase tracking-wider text-amber-300 border border-amber-500/40 animate-pulse shrink-0" title="Device disconnected: Discarding output">
          GHOST
        </span>
      {/if}
      <span class="rounded bg-purple-500/20 px-1.5 py-0.5 text-[9px] font-mono font-bold uppercase tracking-wider text-purple-300 border border-purple-500/40 shrink-0">
        OUTPUT
      </span>
      <span class="rounded px-1.5 py-0.5 text-[9px] font-mono font-semibold uppercase shrink-0 {data.device.is_virtual
        ? 'bg-purple-500/20 text-purple-300 border border-purple-500/30'
        : 'bg-zinc-800 text-zinc-300 border border-zinc-700'}">
        {data.device.is_virtual ? 'VIRT' : 'HW'}
      </span>
      {#if data.onDismiss}
        <button
          type="button"
          onclick={(e) => {
            e.stopPropagation();
            data.onDismiss?.();
          }}
          class="rounded p-0.5 text-zinc-400 hover:text-red-400 hover:bg-red-500/10 transition-colors ml-0.5 cursor-pointer shrink-0"
          title="Remove from canvas"
          aria-label="Remove from canvas"
        >
          <X class="w-3.5 h-3.5" />
        </button>
      {/if}
    </div>
  </div>

  <!-- Hardware Details Bar -->
  <div class="mt-2.5 flex items-center justify-between text-[10px] font-mono text-zinc-400 bg-zinc-950/60 rounded-md px-2 py-1 border border-zinc-800/80 gap-1">
    <span class="text-purple-300/90 truncate max-w-[90px]" title={(data.device as any).node_id || 'local'}>{(data.device as any).node_id || 'local'}</span>
    <span class="text-zinc-300 font-semibold">{data.device.max_output} Out Ch</span>
    <span>{data.device.default_sample_rate / 1000} kHz</span>
    <span class="text-zinc-500">24-bit</span>
  </div>

  <!-- Live Mini Level Meter -->
  <div class="mt-2.5 rounded-md bg-zinc-950/80 p-2 border border-zinc-800/80">
    <div class="flex items-center justify-between text-[9px] font-mono mb-1">
      <span class="text-zinc-400 uppercase tracking-wider font-semibold">Output Monitor</span>
      <span class={data.sourceRooms.length === 0 ? 'text-zinc-500' : 'text-purple-400'}>
        {#if data.sourceRooms.length === 0}
          unrouted (-inf)
        {:else if leftMeter.peakDb > -60}
          RMS {leftMeter.rmsDb > -60 ? leftMeter.rmsDb.toFixed(1) : '-inf'} / Pk {leftMeter.peakDb.toFixed(1)} dB
        {:else}
          -inf dB
        {/if}
      </span>
    </div>
    <!-- Dual Bar Meter L / R -->
    <div class="space-y-1">
      <!-- Channel L -->
      <div class="flex items-center gap-1.5">
        <span class="text-[8px] font-mono text-zinc-500 w-2.5">L</span>
        <div class="h-1.5 flex-1 bg-zinc-800 rounded-sm overflow-hidden relative">
          <div
            class="h-full bg-gradient-to-r from-emerald-500 via-yellow-400 to-purple-500 transition-all duration-75"
            style="width: {leftPeakPercent}%"
          ></div>
        </div>
        {#if leftMeter.clipped}
          <span class="h-1.5 w-1.5 rounded-full bg-red-500 shadow-[0_0_4px_red]"></span>
        {/if}
      </div>
      <!-- Channel R -->
      <div class="flex items-center gap-1.5">
        <span class="text-[8px] font-mono text-zinc-500 w-2.5">R</span>
        <div class="h-1.5 flex-1 bg-zinc-800 rounded-sm overflow-hidden relative">
          <div
            class="h-full bg-gradient-to-r from-emerald-500 via-yellow-400 to-purple-500 transition-all duration-75"
            style="width: {rightPeakPercent}%"
          ></div>
        </div>
        {#if rightMeter.clipped}
          <span class="h-1.5 w-1.5 rounded-full bg-red-500 shadow-[0_0_4px_red]"></span>
        {/if}
      </div>
    </div>
  </div>

  <!-- Device Gain & Mute Control Bar -->
  <div class="mt-2 flex items-center justify-between gap-2 rounded-md bg-zinc-950/60 px-2 py-1.5 border border-zinc-800/80 text-[10px] font-mono">
    <button
      type="button"
      onclick={() => data.onToggleMute?.()}
      class="nodrag rounded p-1 border transition-colors cursor-pointer shrink-0 {data.device.muted
        ? 'bg-red-950/60 text-red-400 border-red-500/40'
        : 'bg-zinc-900 text-purple-400 border-zinc-700 hover:border-purple-500/50'}"
      title={data.device.muted ? 'Unmute device' : 'Mute device'}
      aria-label={data.device.muted ? 'Unmute device' : 'Mute device'}
    >
      {#if data.device.muted}
        <VolumeX class="w-3 h-3" />
      {:else}
        <Volume2 class="w-3 h-3" />
      {/if}
    </button>
    <input
      type="range"
      min="0"
      max="1.5"
      step="0.05"
      value={data.device.volume ?? 1.0}
      oninput={(e) => data.onVolumeChange?.(parseFloat((e.currentTarget as HTMLInputElement).value))}
      onchange={(e) => data.onVolumeChange?.(parseFloat((e.currentTarget as HTMLInputElement).value))}
      class="nodrag flex-1 h-1 bg-zinc-800 rounded-lg appearance-none cursor-pointer accent-purple-400"
      aria-label="Device gain for {data.device.name}"
    />
    <span class="w-9 text-right text-zinc-300 shrink-0">{Math.round((data.device.volume ?? 1.0) * 100)}%</span>
    {#if data.device.is_virtual && data.onDeleteVirtual}
      <button
        type="button"
        onclick={() => data.onDeleteVirtual?.()}
        class="nodrag rounded p-1 text-zinc-500 hover:text-red-400 hover:bg-red-950/30 transition-colors cursor-pointer shrink-0"
        title="Delete virtual device"
        aria-label="Delete virtual device"
      >
        <Trash2 class="w-3 h-3" />
      </button>
    {/if}
  </div>

  <!-- Source Rooms (Rooms feeding this output) -->
  <div class="mt-2.5 pt-2 border-t border-zinc-800/80">
    <div class="flex items-center justify-between mb-1.5">
      <div class="flex items-center gap-1 text-[10px] font-mono text-zinc-400 uppercase font-semibold tracking-wider">
        <Layers class="w-3 h-3 text-purple-400" />
        <span>Fed By Rooms ({data.sourceRooms.length})</span>
      </div>

      <!-- Quick Add from Room Button -->
      {#if availableRooms.length > 0}
        <div class="relative">
          <button
            type="button"
            onclick={() => (showAddMenu = !showAddMenu)}
            class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[9px] font-mono font-semibold bg-purple-500/10 text-purple-300 border border-purple-500/30 hover:bg-purple-500/20 transition-colors"
            title="Route another room into this output"
          >
            <Plus class="w-2.5 h-2.5" />
            <span>Room</span>
          </button>

          <!-- Dropdown menu -->
          {#if showAddMenu}
            <div class="absolute right-0 top-6 z-50 w-44 rounded-lg border border-zinc-700 bg-zinc-900 p-1 shadow-xl text-left">
              <div class="px-2 py-1 text-[9px] font-mono text-zinc-400 border-b border-zinc-800">
                Select source room:
              </div>
              {#each availableRooms as room}
                <button
                  type="button"
                  onclick={() => {
                    data.onAddToRoom(room.id);
                    showAddMenu = false;
                  }}
                  class="w-full text-left rounded px-2 py-1 text-xs font-mono text-zinc-200 hover:bg-purple-500/20 hover:text-purple-300 transition-colors truncate"
                >
                  {room.name}
                </button>
              {/each}
            </div>
          {/if}
        </div>
      {/if}
    </div>

    <!-- Active Source Room Tags -->
    {#if data.sourceRooms.length > 0}
      <div class="flex flex-wrap gap-1 max-h-20 overflow-y-auto pr-0.5">
        {#each data.sourceRooms as room}
          <span class="inline-flex items-center gap-1 rounded bg-purple-950/40 border border-purple-500/30 px-1.5 py-0.5 text-[10px] font-mono text-purple-300 max-w-full">
            <span class="truncate max-w-[200px]">{room.name}</span>
            <button
              type="button"
              onclick={() => data.onRemoveFromRoom(room.id)}
              class="text-zinc-400 hover:text-red-400 p-0.5 transition-colors shrink-0 cursor-pointer"
              title="Disconnect from {room.name}"
              aria-label="Disconnect from {room.name}"
            >
              <X class="w-2.5 h-2.5" />
            </button>
          </span>
        {/each}
      </div>
    {:else}
      <p class="text-[10px] font-mono text-zinc-500 italic">
        Output idle. Drag wire from Room &rarr; to connect!
      </p>
    {/if}
  </div>
</div>
