<script lang="ts">
  import { Handle, Position } from '@xyflow/svelte';
  import { Switch } from 'bits-ui';
  import { LATENCY_PRESETS, type RoomInfo, type AudioDevice } from '$lib/api/types';
  import { roomStore } from '$lib/stores/rooms.svelte';
  import Crown from '@lucide/svelte/icons/crown';
  import Network from '@lucide/svelte/icons/network';
  import Plus from '@lucide/svelte/icons/plus';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import X from '@lucide/svelte/icons/x';
  import Zap from '@lucide/svelte/icons/zap';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';

  interface RoomBusData {
    room: RoomInfo;
    inputDevices: AudioDevice[];
    outputDevices: AudioDevice[];
    availableInputs: AudioDevice[];
    availableOutputs: AudioDevice[];
    onToggleSync: (synced: boolean) => void;
    onAddInput: (devId: string) => void;
    onRemoveInput: (devId: string) => void;
    onAddOutput: (devId: string) => void;
    onRemoveOutput: (devId: string) => void;
    onDeleteRoom: () => void;
  }

  let { id, data, selected = false }: { id: string; data: RoomBusData; selected?: boolean } = $props();

  let showAddInputMenu = $state(false);
  let showAddOutputMenu = $state(false);

  const activeLinkCount = $derived(
    (data.room.inputs?.length || 0) * (data.room.outputs?.length || 0)
  );
  const currentPreset = $derived(data.room.latency_preset || 'balanced');
  const currentPresetHint = $derived(
    LATENCY_PRESETS.find((p) => p.id === currentPreset)?.hint || '20ms packet / 20–200ms jitter'
  );
</script>

<div
  class="relative w-[420px] rounded-2xl border bg-gradient-to-b from-zinc-900/95 via-zinc-900/90 to-zinc-950/95 backdrop-blur-md p-4 shadow-2xl transition-all select-none {selected
    ? 'border-cyan-400 ring-2 ring-cyan-500/40 shadow-cyan-500/25'
    : 'border-zinc-700/80 hover:border-zinc-600 shadow-black/80'}"
>
  <!-- Svelte Flow Target Handle (Inputs feed into the Room) -->
  <Handle
    type="target"
    position={Position.Left}
    id="in"
    class="!w-4 !h-4 !bg-cyan-400 !border-2 !border-zinc-950 !rounded-full shadow-[0_0_10px_rgba(6,182,212,0.9)] hover:shadow-[0_0_16px_rgba(6,182,212,1)] hover:brightness-125 hover:ring-2 hover:ring-cyan-300/80 transition-all cursor-crosshair"
    title="Drop connection here to assign input device to this room"
  />

  <!-- Svelte Flow Source Handle (Room feeds into Outputs) -->
  <Handle
    type="source"
    position={Position.Right}
    id="out"
    class="!w-4 !h-4 !bg-purple-400 !border-2 !border-zinc-950 !rounded-full shadow-[0_0_10px_rgba(168,85,247,0.9)] hover:shadow-[0_0_16px_rgba(168,85,247,1)] hover:brightness-125 hover:ring-2 hover:ring-purple-300/80 transition-all cursor-crosshair"
    title="Drag wire to route this room into an output device"
  />

  <!-- Header -->
  <div class="flex items-start justify-between gap-3 border-b border-zinc-800/80 pb-3">
    <div class="flex items-center gap-2.5 min-w-0">
      <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-gradient-to-br from-cyan-500/20 to-purple-500/20 border border-cyan-500/40 text-cyan-300 shadow-sm shrink-0">
        <Network class="w-4 h-4" />
      </div>
      <div class="min-w-0">
        <div class="flex items-center gap-2 min-w-0">
          <h2 class="text-sm font-bold text-zinc-100 truncate flex-1 min-w-0" title={data.room.name}>
            {data.room.name}
          </h2>
          <span class="rounded px-1.5 py-0.2 text-[9px] font-mono font-bold uppercase tracking-wider shrink-0 {data.room.state === 'active'
            ? 'bg-emerald-500/20 text-emerald-400 border border-emerald-500/30'
            : 'bg-zinc-800 text-zinc-400 border border-zinc-700'}">
            {data.room.state}
          </span>
        </div>
        <span class="text-[10px] font-mono text-zinc-400 truncate block">
          ID: {data.room.id}
        </span>
      </div>
    </div>

    <!-- Actions (Delete) -->
    <button
      type="button"
      onclick={() => data.onDeleteRoom()}
      class="rounded-lg p-1.5 text-zinc-500 hover:text-red-400 hover:bg-red-950/30 transition-colors shrink-0"
      title="Delete Room"
      aria-label="Delete Room"
    >
      <Trash2 class="w-3.5 h-3.5" />
    </button>
  </div>

  <!-- Mesh Peer Sync Switch (Bits UI Switch) -->
  <div class="mt-3 flex items-center justify-between rounded-xl bg-zinc-950/80 p-2.5 border border-zinc-800/90">
    <div class="flex flex-col">
      <div class="flex items-center gap-1.5">
        <Zap class="w-3.5 h-3.5 {data.room.synced ? 'text-cyan-400' : 'text-zinc-500'}" />
        <span class="text-xs font-semibold font-mono text-zinc-200">Mesh Peer Sync</span>
      </div>
      <span class="text-[10px] font-mono {data.room.synced ? 'text-emerald-400 font-bold' : 'text-zinc-500'}">
        {data.room.synced ? 'SYNCHRONIZED (All P2P Peers)' : 'LOCAL (This node only)'}
      </span>
    </div>

    <!-- Bits UI Switch Component -->
    <div class="flex items-center gap-2">
      <Switch.Root
        checked={data.room.synced}
        onCheckedChange={(checked) => data.onToggleSync(checked)}
        class="relative inline-flex h-5 w-10 shrink-0 cursor-pointer rounded-full border border-zinc-700 bg-zinc-800 transition-colors duration-200 ease-in-out focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-500 data-[state=checked]:bg-cyan-500 data-[state=checked]:border-cyan-400"
        aria-label="Toggle Room Mesh Synchronization"
      >
        <Switch.Thumb
          class="pointer-events-none inline-block h-4 w-4 transform rounded-full bg-zinc-200 shadow-md transition duration-200 ease-in-out data-[state=checked]:translate-x-5 data-[state=checked]:bg-zinc-950 data-[state=unchecked]:translate-x-0.5"
        />
      </Switch.Root>
    </div>
  </div>

  <!-- Latency Preset Selector -->
  <div class="mt-3 rounded-xl bg-zinc-950/80 p-2.5 border border-zinc-800/90">
    <div class="flex items-center justify-between mb-2">
      <span class="text-[10px] font-mono text-zinc-400">LATENCY PRESET</span>
      <span class="text-[10px] font-mono text-cyan-300">{currentPresetHint}</span>
    </div>
    <div class="grid grid-cols-3 gap-1">
      {#each LATENCY_PRESETS as preset}
        <button
          type="button"
          onclick={() => roomStore.setRoomLatency(data.room.id, preset.id)}
          class="rounded-md px-2 py-1 text-[10px] font-mono font-semibold uppercase tracking-wide border transition-colors cursor-pointer {currentPreset === preset.id
            ? 'bg-cyan-500 text-zinc-950 border-cyan-400'
            : 'bg-zinc-900 text-zinc-300 border-zinc-700 hover:border-cyan-500/50 hover:text-cyan-300'}"
          title={preset.hint}
        >
          {preset.label}
        </button>
      {/each}
    </div>
  </div>

  <!-- All-to-All Summing Bus Core Indicator -->
  <div class="mt-3 rounded-xl bg-gradient-to-r from-cyan-950/30 via-zinc-950/70 to-purple-950/30 p-2.5 border border-cyan-500/20">
    <div class="flex flex-wrap items-center justify-between gap-1.5 text-[11px] font-mono">
      <span class="text-zinc-300 font-semibold flex items-center gap-1 shrink-0">
        <span>Summing Audio Bus</span>
        <ArrowRight class="w-3 h-3 text-cyan-400" />
      </span>
      <span class="rounded bg-cyan-500/20 px-2 py-0.5 text-[10px] font-mono font-bold text-cyan-300 border border-cyan-500/40 shrink-0">
        {activeLinkCount} Active Paths ({data.room.inputs?.length || 0} In &times; {data.room.outputs?.length || 0} Out)
      </span>
    </div>
    <p class="mt-1 text-[10px] font-sans text-zinc-400 leading-snug">
      Every input assigned below automatically sums and plays to every assigned output.
    </p>
  </div>

  <!-- Two Column Routing Section: Inputs vs Outputs -->
  <div class="mt-3 grid grid-cols-2 gap-3">
    <!-- Assigned Inputs Column -->
    <div class="rounded-xl bg-zinc-950/70 p-2.5 border border-zinc-800/80">
      <div class="flex items-center justify-between mb-2">
        <span class="text-[10px] font-mono font-bold uppercase tracking-wider text-cyan-400">
          Inputs ({data.room.inputs?.length || 0})
        </span>

        <!-- + Add Input Button -->
        {#if data.availableInputs.length > 0}
          <div class="relative">
            <button
              type="button"
              onclick={() => (showAddInputMenu = !showAddInputMenu)}
              class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[9px] font-mono font-semibold bg-cyan-500/10 text-cyan-300 border border-cyan-500/30 hover:bg-cyan-500/20 transition-colors"
              title="Add an input to this room"
            >
              <Plus class="w-2.5 h-2.5" />
              <span>Input</span>
            </button>

            {#if showAddInputMenu}
              <div class="absolute left-0 top-6 z-50 w-44 rounded-lg border border-zinc-700 bg-zinc-900 p-1 shadow-xl text-left">
                <div class="px-2 py-1 text-[9px] font-mono text-zinc-400 border-b border-zinc-800">
                  Select input device:
                </div>
                {#each data.availableInputs as dev}
                  <button
                    type="button"
                    onclick={() => {
                      const devKey = (dev as any).node_id && !dev.id.includes('/') ? `${(dev as any).node_id}/${dev.id}` : dev.id;
                      data.onAddInput(devKey);
                      showAddInputMenu = false;
                    }}
                    class="w-full text-left rounded px-2 py-1 text-xs font-mono text-zinc-200 hover:bg-cyan-500/20 hover:text-cyan-300 transition-colors truncate"
                  >
                    {dev.name}
                  </button>
                {/each}
              </div>
            {/if}
          </div>
        {/if}
      </div>

      <!-- Inputs List -->
      <div class="space-y-1.5 max-h-36 overflow-y-auto pr-1">
        {#each data.inputDevices as dev ((dev as any).endpoint_id || dev.id)}
          <div class="flex items-center justify-between min-w-0 rounded bg-zinc-900/90 border border-cyan-500/30 px-2 py-1 text-[10px] font-mono text-zinc-200">
            <div class="truncate mr-1 min-w-0 flex-1" title={dev.name}>
              <span class="text-cyan-300 font-medium truncate block">{dev.name}</span>
            </div>
            <button
              type="button"
              onclick={() => data.onRemoveInput((dev as any).endpoint_id || dev.id)}
              class="text-zinc-500 hover:text-red-400 p-0.5 transition-colors shrink-0 cursor-pointer"
              title="Remove {dev.name} from room"
              aria-label="Remove {dev.name} from room"
            >
              <X class="w-3 h-3" />
            </button>
          </div>
        {/each}
        {#if data.inputDevices.length === 0}
          <div class="text-[10px] font-mono text-zinc-500 italic p-1 text-center">
            No inputs yet. Connect wire to left handle &larr;
          </div>
        {/if}
      </div>
    </div>

    <!-- Assigned Outputs Column -->
    <div class="rounded-xl bg-zinc-950/70 p-2.5 border border-zinc-800/80">
      <div class="flex items-center justify-between mb-2">
        <span class="text-[10px] font-mono font-bold uppercase tracking-wider text-purple-400">
          Outputs ({data.room.outputs?.length || 0})
        </span>

        <!-- + Add Output Button -->
        {#if data.availableOutputs.length > 0}
          <div class="relative">
            <button
              type="button"
              onclick={() => (showAddOutputMenu = !showAddOutputMenu)}
              class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[9px] font-mono font-semibold bg-purple-500/10 text-purple-300 border border-purple-500/30 hover:bg-purple-500/20 transition-colors"
              title="Add an output to this room"
            >
              <Plus class="w-2.5 h-2.5" />
              <span>Output</span>
            </button>

            {#if showAddOutputMenu}
              <div class="absolute right-0 top-6 z-50 w-44 rounded-lg border border-zinc-700 bg-zinc-900 p-1 shadow-xl text-left">
                <div class="px-2 py-1 text-[9px] font-mono text-zinc-400 border-b border-zinc-800">
                  Select output device:
                </div>
                {#each data.availableOutputs as dev}
                  <button
                    type="button"
                    onclick={() => {
                      const devKey = (dev as any).node_id && !dev.id.includes('/') ? `${(dev as any).node_id}/${dev.id}` : dev.id;
                      data.onAddOutput(devKey);
                      showAddOutputMenu = false;
                    }}
                    class="w-full text-left rounded px-2 py-1 text-xs font-mono text-zinc-200 hover:bg-purple-500/20 hover:text-purple-300 transition-colors truncate"
                  >
                    {dev.name}
                  </button>
                {/each}
              </div>
            {/if}
          </div>
        {/if}
      </div>

      <!-- Outputs List -->
      <div class="space-y-1.5 max-h-36 overflow-y-auto pr-1">
        {#each data.outputDevices as dev ((dev as any).endpoint_id || dev.id)}
          <div class="flex items-center justify-between min-w-0 rounded bg-zinc-900/90 border border-purple-500/30 px-2 py-1 text-[10px] font-mono text-zinc-200">
            <div class="truncate mr-1 min-w-0 flex-1" title={dev.name}>
              <span class="text-purple-300 font-medium truncate block">{dev.name}</span>
            </div>
            <button
              type="button"
              onclick={() => data.onRemoveOutput((dev as any).endpoint_id || dev.id)}
              class="text-zinc-500 hover:text-red-400 p-0.5 transition-colors shrink-0 cursor-pointer"
              title="Remove {dev.name} from room"
              aria-label="Remove {dev.name} from room"
            >
              <X class="w-3 h-3" />
            </button>
          </div>
        {/each}
        {#if data.outputDevices.length === 0}
          <div class="text-[10px] font-mono text-zinc-500 italic p-1 text-center">
            No outputs yet. Drag right handle to output &rarr;
          </div>
        {/if}
      </div>
    </div>
  </div>

  <!-- Room Meta Footer -->
  <div class="mt-3 flex items-center justify-between pt-2 border-t border-zinc-800/80 text-[10px] font-mono text-zinc-400 min-w-0 gap-2">
    <div class="flex items-center gap-1 min-w-0 flex-1">
      <Crown class="w-3 h-3 text-amber-400 shrink-0" />
      <span class="truncate">Leader: <strong class="text-amber-300 font-bold">{data.room.leader || 'node-local'}</strong></span>
    </div>
    <div class="flex items-center gap-1.5 shrink-0">
      <span>{data.room.members?.length || 1} peer node(s)</span>
    </div>
  </div>
</div>
