<script lang="ts">
  import { onMount } from 'svelte';
  import { roomStore } from '$lib/stores/rooms.svelte';
  import { peerStore } from '$lib/stores/peers.svelte';
  import { LATENCY_PRESETS, latencyPresetLabel } from '$lib/api/types';
  import Network from '@lucide/svelte/icons/network';
  import Crown from '@lucide/svelte/icons/crown';
  import Plus from '@lucide/svelte/icons/plus';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Clock from '@lucide/svelte/icons/clock';
  import Radio from '@lucide/svelte/icons/radio';
  import X from '@lucide/svelte/icons/x';
  import Zap from '@lucide/svelte/icons/zap';
  import { Switch } from 'bits-ui';

  let isCreateModalOpen = $state(false);
  let isPeerModalOpen = $state(false);

  // Form state for creating room
  let newRoomId = $state('room-broadcast-b');
  let newRoomName = $state('Broadcast Stage B');
  let isSubmitting = $state(false);
  let roomError = $state<string | null>(null);

  // Form state for connecting unicast peer
  let peerAddr = $state('192.168.1.150:7001');
  let peerTlsFingerprint = $state('');
  let isConnectingPeer = $state(false);
  let peerError = $state<string | null>(null);

  onMount(() => {
    roomStore.loadRooms();
    peerStore.loadPeers();
    peerStore.loadClockSync();
  });

  async function handleCreateRoom() {
    isSubmitting = true;
    roomError = null;
    try {
      await roomStore.createRoom({
        id: newRoomId.trim().toLowerCase().replace(/\s+/g, '-'),
        name: newRoomName.trim(),
      });
      isCreateModalOpen = false;
      newRoomId = 'room-' + Math.floor(Math.random() * 1000);
      newRoomName = 'Audio Stage ' + Math.floor(Math.random() * 100);
    } catch (err: any) {
      roomError = err.message || 'Failed to create conference room';
    } finally {
      isSubmitting = false;
    }
  }

  async function handleConnectPeer() {
    isConnectingPeer = true;
    peerError = null;
    try {
      await peerStore.connectPeer(peerAddr.trim(), peerTlsFingerprint.trim() || undefined);
      isPeerModalOpen = false;
    } catch (err: any) {
      peerError = err.message || 'Failed to initiate peer handshake';
    } finally {
      isConnectingPeer = false;
    }
  }

  function toggleRoomJoin(roomId: string) {
    if (roomStore.activeJoinedRoomId === roomId) {
      roomStore.leaveRoom(roomId);
    } else {
      roomStore.joinRoom(roomId);
    }
  }

  function handleDeleteRoom(roomId: string) {
    if (confirm(`Delete audio conference room "${roomId}"?`)) {
      roomStore.deleteRoom(roomId);
    }
  }
</script>

<div class="flex flex-col h-full bg-studio-dark select-none overflow-hidden">
  <!-- Toolbar -->
  <div class="flex items-center justify-between border-b border-zinc-800 bg-zinc-950/70 px-4 py-2.5">
    <div class="flex items-center gap-3">
      <div class="flex items-center gap-2">
        <Network class="w-4 h-4 text-cyan-400" />
        <h1 class="text-sm font-semibold tracking-wide text-zinc-100 uppercase">
          Mesh Rooms & Peer Coordinator
        </h1>
      </div>
      <span class="rounded bg-zinc-800 px-2 py-0.5 text-xs font-mono text-zinc-300">
        {roomStore.rooms.length} Rooms &bull; {peerStore.peers.length} Mesh Peers
      </span>
    </div>

    <!-- Actions -->
    <div class="flex items-center gap-2">
      <button
        onclick={() => (isPeerModalOpen = true)}
        class="inline-flex items-center gap-1.5 rounded-lg border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-xs font-medium text-zinc-200 hover:bg-zinc-800 transition-colors"
      >
        <Radio class="w-3.5 h-3.5 text-cyan-400" />
        <span>Pair Remote Peer</span>
      </button>

      <button
        onclick={() => (isCreateModalOpen = true)}
        class="inline-flex items-center gap-1.5 rounded-lg bg-cyan-500 px-3 py-1.5 text-xs font-semibold text-zinc-950 shadow-sm shadow-cyan-500/30 hover:bg-cyan-400 transition-colors"
      >
        <Plus class="w-3.5 h-3.5" />
        <span>Create Conference Room</span>
      </button>

      <button
        onclick={() => {
          roomStore.loadRooms();
          peerStore.loadPeers();
          peerStore.loadClockSync();
        }}
        class="inline-flex items-center justify-center h-7.5 w-7.5 rounded-md border border-zinc-800 bg-zinc-900 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-800 transition-colors"
        title="Refresh Rooms & Peers"
      >
        <RefreshCw class="w-3.5 h-3.5 {roomStore.isLoading ? 'animate-spin text-cyan-400' : ''}" />
      </button>
    </div>
  </div>

  <!-- Content -->
  <div class="flex-1 overflow-y-auto p-6 bg-zinc-950">
    <div class="max-w-6xl mx-auto space-y-8">
      <!-- Conference Rooms Section -->
      <div>
        <div class="flex items-center justify-between mb-4">
          <div class="flex items-center gap-2">
            <Network class="w-4 h-4 text-cyan-400" />
            <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-300">
              Active Audio Conference Rooms
            </h2>
          </div>
          <span class="text-xs font-mono text-zinc-500">
            Multi-node synchronized low-latency bus
          </span>
        </div>

        <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {#each roomStore.rooms as room}
            {@const isJoined = roomStore.activeJoinedRoomId === room.id}
            {@const currentPreset = room.latency_preset || 'balanced'}
            <div class="rounded-xl border transition-all p-5 shadow-xl flex flex-col justify-between {isJoined
              ? 'border-cyan-500/60 bg-gradient-to-b from-cyan-950/20 to-zinc-900/90 shadow-cyan-950/30'
              : 'border-zinc-800 bg-zinc-900/60 hover:border-zinc-700'}">
              <div>
                <!-- Top Badge & Delete -->
                <div class="flex items-center justify-between">
                  <span class="rounded px-2 py-0.5 text-[10px] font-mono font-bold uppercase tracking-wide border {room.state === 'active'
                    ? 'bg-emerald-500/10 text-emerald-400 border-emerald-500/30'
                    : 'bg-zinc-800 text-zinc-400 border-zinc-700'}">
                    {room.state}
                  </span>

                  <button
                    onclick={() => handleDeleteRoom(room.id)}
                    class="rounded p-1 text-zinc-500 hover:text-red-400 hover:bg-red-950/30 transition-colors"
                    title="Delete Room"
                  >
                    <Trash2 class="w-3.5 h-3.5" />
                  </button>
                </div>

                <!-- Room Title & ID -->
                <h3 class="mt-3 text-base font-bold text-zinc-100 flex items-center gap-2">
                  <span>{room.name}</span>
                </h3>
                <span class="text-[11px] font-mono text-zinc-500 block">{room.id}</span>

                <!-- Room Leader Pill -->
                <div class="mt-4 flex items-center gap-2 rounded-lg bg-zinc-950/70 p-2.5 border border-zinc-800/80">
                  <Crown class="w-4 h-4 text-amber-400 shrink-0" />
                  <div class="flex flex-col truncate">
                    <span class="text-[10px] font-mono text-zinc-400">CLOCK LEADER</span>
                    <span class="text-xs font-mono font-bold text-amber-300 truncate">
                      {room.leader || 'node-local'}
                    </span>
                  </div>
                </div>

                <!-- Mesh Peer Sync Switch (Bits UI Switch) -->
                <div class="mt-3 flex items-center justify-between rounded-lg bg-zinc-950/70 p-2.5 border border-zinc-800/80">
                  <div class="flex flex-col">
                    <span class="text-[10px] font-mono text-zinc-400">P2P MESH SYNC</span>
                    <span class="text-xs font-mono font-bold {room.synced ? 'text-emerald-400' : 'text-zinc-500'}">
                      {room.synced ? 'SYNC ENABLED' : 'LOCAL BUS ONLY'}
                    </span>
                  </div>
                  <Switch.Root
                    checked={room.synced}
                    onCheckedChange={(checked) => roomStore.setRoomSync(room.id, checked)}
                    class="relative inline-flex h-5 w-10 shrink-0 cursor-pointer rounded-full border border-zinc-700 bg-zinc-800 transition-colors duration-200 ease-in-out focus:outline-none data-[state=checked]:bg-cyan-500"
                    aria-label="Toggle Mesh Sync"
                  >
                    <Switch.Thumb
                      class="pointer-events-none inline-block h-4 w-4 transform rounded-full bg-zinc-200 shadow-md transition duration-200 ease-in-out data-[state=checked]:translate-x-5 data-[state=checked]:bg-zinc-950 data-[state=unchecked]:translate-x-0.5"
                    />
                  </Switch.Root>
                </div>

                <!-- Latency Preset Selector -->
                <div class="mt-3 rounded-lg bg-zinc-950/70 p-2.5 border border-zinc-800/80">
                  <div class="flex items-center justify-between mb-2">
                    <span class="text-[10px] font-mono text-zinc-400">LATENCY PRESET</span>
                    <span class="text-[10px] font-mono text-cyan-300">{latencyPresetLabel(currentPreset).hint}</span>
                  </div>
                  <div class="grid grid-cols-3 gap-1">
                    {#each LATENCY_PRESETS as preset}
                      <button
                        onclick={() => roomStore.setRoomLatency(room.id, preset.id)}
                        class="rounded-md px-2 py-1 text-[10px] font-mono font-semibold uppercase tracking-wide border transition-colors {currentPreset === preset.id
                          ? 'bg-cyan-500 text-zinc-950 border-cyan-400'
                          : 'bg-zinc-900 text-zinc-300 border-zinc-700 hover:border-cyan-500/50 hover:text-cyan-300'}"
                        title={preset.hint}
                      >
                        {preset.label}
                      </button>
                    {/each}
                  </div>
                </div>

                <!-- Assigned Audio Devices Bus -->
                <div class="mt-3 rounded-lg bg-zinc-950/60 p-2.5 border border-zinc-800/80">
                  <div class="flex items-center justify-between text-[10px] font-mono mb-2">
                    <span class="text-zinc-400">SUMMING AUDIO BUS</span>
                    <span class="text-cyan-300 font-bold">
                      {(room.inputs?.length || 0) * (room.outputs?.length || 0)} Active Links
                    </span>
                  </div>
                  <div class="grid grid-cols-2 gap-2 text-[10px] font-mono">
                    <div>
                      <span class="text-cyan-400 block mb-1">Inputs ({room.inputs?.length || 0}):</span>
                      <div class="flex flex-wrap gap-1">
                        {#each room.inputs || [] as inDev}
                          <span class="rounded bg-zinc-900 border border-cyan-500/30 px-1.5 py-0.5 text-zinc-300 truncate max-w-full">
                            {inDev}
                          </span>
                        {/each}
                        {#if !room.inputs || room.inputs.length === 0}
                          <span class="text-zinc-500 italic">None</span>
                        {/if}
                      </div>
                    </div>
                    <div>
                      <span class="text-purple-400 block mb-1">Outputs ({room.outputs?.length || 0}):</span>
                      <div class="flex flex-wrap gap-1">
                        {#each room.outputs || [] as outDev}
                          <span class="rounded bg-zinc-900 border border-purple-500/30 px-1.5 py-0.5 text-zinc-300 truncate max-w-full">
                            {outDev}
                          </span>
                        {/each}
                        {#if !room.outputs || room.outputs.length === 0}
                          <span class="text-zinc-500 italic">None</span>
                        {/if}
                      </div>
                    </div>
                  </div>
                </div>

                <!-- Members List -->
                <div class="mt-3">
                  <div class="text-[10px] font-mono text-zinc-400 mb-1.5 flex justify-between">
                    <span>PARTICIPANTS</span>
                    <span class="text-cyan-400">{room.members?.length || 0} nodes</span>
                  </div>
                  <div class="flex flex-wrap gap-1.5 max-h-24 overflow-y-auto">
                    {#each room.members || [] as member}
                      <span class="inline-flex items-center gap-1 rounded bg-zinc-800/80 px-2 py-0.5 text-[10px] font-mono text-zinc-300 border border-zinc-700/50">
                        {#if member === room.leader}
                          <Crown class="w-2.5 h-2.5 text-amber-400" />
                        {/if}
                        <span>{member}</span>
                      </span>
                    {/each}
                  </div>
                </div>
              </div>

              <!-- Join / Leave Button -->
              <div class="mt-5 pt-3 border-t border-zinc-800/80 flex items-center justify-between">
                <div class="flex items-center gap-1 text-[11px] font-mono text-zinc-400">
                  <Clock class="w-3.5 h-3.5 text-cyan-400" />
                  <span>Sync: &le; 1ms</span>
                </div>

                <button
                  onclick={() => toggleRoomJoin(room.id)}
                  class="rounded-lg px-4 py-1.5 text-xs font-semibold font-mono transition-all {isJoined
                    ? 'bg-red-500/20 text-red-300 border border-red-500/40 hover:bg-red-500/30'
                    : 'bg-cyan-500 text-zinc-950 hover:bg-cyan-400 shadow-sm shadow-cyan-500/30'}"
                >
                  {isJoined ? 'Leave Room' : 'Join Room'}
                </button>
              </div>
            </div>
          {/each}
        </div>
      </div>

      <!-- Discovered Mesh Peers Section -->
      <div>
        <div class="flex items-center justify-between mb-4">
          <div class="flex items-center gap-2">
            <Radio class="w-4 h-4 text-cyan-400" />
            <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-300">
              Discovered Mesh Peers & PTP Sync
            </h2>
          </div>
          <span class="text-xs font-mono text-zinc-500">
            mDNS & DHT Peer Discovery
          </span>
        </div>

        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 overflow-hidden shadow-xl">
          <table class="w-full text-left text-xs font-mono">
            <thead class="border-b border-zinc-800 bg-zinc-950/80 text-zinc-400 text-[10px] uppercase">
              <tr>
                <th class="px-4 py-3">Peer Node ID</th>
                <th class="px-4 py-3">Network Address</th>
                <th class="px-4 py-3">Status</th>
                <th class="px-4 py-3">RTT Latency</th>
                <th class="px-4 py-3">Clock Offset</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-zinc-800/60 text-zinc-200">
              {#each peerStore.peers as peer}
                <tr class="hover:bg-zinc-800/30 transition-colors">
                  <td class="px-4 py-3 font-semibold text-zinc-100 flex items-center gap-2">
                    <span class="h-2 w-2 rounded-full {peer.status === 'connected' ? 'bg-emerald-400' : 'bg-amber-400'}"></span>
                    <span>{peer.id}</span>
                  </td>
                  <td class="px-4 py-3 text-zinc-400">{peer.addr}</td>
                  <td class="px-4 py-3">
                    <span class="rounded px-2 py-0.5 text-[10px] font-bold uppercase {peer.status === 'connected'
                      ? 'bg-emerald-500/10 text-emerald-400 border border-emerald-500/20'
                      : 'bg-amber-500/10 text-amber-400 border border-amber-500/20'}">
                      {peer.status}
                    </span>
                  </td>
                  <td class="px-4 py-3 text-cyan-400 font-bold">
                    {peer.rtt_ms !== undefined ? `${peer.rtt_ms.toFixed(1)} ms` : '--'}
                  </td>
                  <td class="px-4 py-3 text-zinc-300">
                    {peerStore.clockSync.offset_ns / 1000} &mu;s
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  </div>
</div>

<!-- Create Room Modal -->
{#if isCreateModalOpen}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/80 backdrop-blur-sm p-4"
    role="dialog"
    aria-modal="true"
  >
    <div class="w-full max-w-md rounded-xl border border-zinc-800 bg-zinc-950 p-6 shadow-2xl text-zinc-100">
      <div class="flex items-center justify-between border-b border-zinc-800 pb-3 mb-4">
        <div class="flex items-center gap-2">
          <Network class="w-4 h-4 text-cyan-400" />
          <h2 class="text-sm font-semibold text-zinc-100 uppercase tracking-wide">
            Create Conference Room
          </h2>
        </div>
        <button
          onclick={() => (isCreateModalOpen = false)}
          class="rounded p-1 text-zinc-400 hover:text-zinc-200"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <div class="space-y-4">
        <div>
          <label for="room-id" class="block text-xs font-mono text-zinc-300 mb-1">ROOM IDENTIFIER</label>
          <input
            id="room-id"
            type="text"
            bind:value={newRoomId}
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 font-mono focus:border-cyan-500 focus:outline-none"
          />
        </div>

        <div>
          <label for="room-name" class="block text-xs font-mono text-zinc-300 mb-1">ROOM DISPLAY NAME</label>
          <input
            id="room-name"
            type="text"
            bind:value={newRoomName}
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 focus:border-cyan-500 focus:outline-none"
          />
        </div>

        {#if roomError}
          <div class="text-xs text-red-400 bg-red-950/40 p-2 rounded border border-red-800/50">
            {roomError}
          </div>
        {/if}
      </div>

      <div class="mt-6 flex justify-end gap-2 border-t border-zinc-800 pt-4">
        <button
          onclick={() => (isCreateModalOpen = false)}
          class="rounded-lg px-3 py-1.5 text-xs text-zinc-400 hover:text-zinc-200"
        >
          Cancel
        </button>
        <button
          onclick={handleCreateRoom}
          disabled={isSubmitting}
          class="rounded-lg bg-cyan-500 px-4 py-1.5 text-xs font-semibold text-zinc-950 hover:bg-cyan-400 disabled:opacity-50"
        >
          {isSubmitting ? 'Creating...' : 'Create Room'}
        </button>
      </div>
    </div>
  </div>
{/if}

<!-- Pair Remote Peer Modal -->
{#if isPeerModalOpen}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/80 backdrop-blur-sm p-4"
    role="dialog"
    aria-modal="true"
  >
    <div class="w-full max-w-md rounded-xl border border-zinc-800 bg-zinc-950 p-6 shadow-2xl text-zinc-100">
      <div class="flex items-center justify-between border-b border-zinc-800 pb-3 mb-4">
        <div class="flex items-center gap-2">
          <Radio class="w-4 h-4 text-cyan-400" />
          <h2 class="text-sm font-semibold text-zinc-100 uppercase tracking-wide">
            Pair Remote Mesh Peer
          </h2>
        </div>
        <button
          onclick={() => (isPeerModalOpen = false)}
          class="rounded p-1 text-zinc-400 hover:text-zinc-200"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <div class="space-y-4">
        <div>
          <label for="peer-addr" class="block text-xs font-mono text-zinc-300 mb-1">PEER ADDRESS (IP:PORT)</label>
          <input
            id="peer-addr"
            type="text"
            bind:value={peerAddr}
            placeholder="192.168.1.150:7001"
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 font-mono focus:border-cyan-500 focus:outline-none"
          />
        </div>

        <div>
          <label for="peer-cert" class="block text-xs font-mono text-zinc-300 mb-1">DTLS CERTIFICATE FINGERPRINT (OPTIONAL)</label>
          <input
            id="peer-cert"
            type="text"
            bind:value={peerTlsFingerprint}
            placeholder="SHA-256 fingerprint hex..."
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 font-mono focus:border-cyan-500 focus:outline-none"
          />
        </div>

        {#if peerError}
          <div class="text-xs text-red-400 bg-red-950/40 p-2 rounded border border-red-800/50">
            {peerError}
          </div>
        {/if}
      </div>

      <div class="mt-6 flex justify-end gap-2 border-t border-zinc-800 pt-4">
        <button
          onclick={() => (isPeerModalOpen = false)}
          class="rounded-lg px-3 py-1.5 text-xs text-zinc-400 hover:text-zinc-200"
        >
          Cancel
        </button>
        <button
          onclick={handleConnectPeer}
          disabled={isConnectingPeer}
          class="rounded-lg bg-cyan-500 px-4 py-1.5 text-xs font-semibold text-zinc-950 hover:bg-cyan-400 disabled:opacity-50"
        >
          {isConnectingPeer ? 'Connecting...' : 'Initiate Handshake'}
        </button>
      </div>
    </div>
  </div>
{/if}
