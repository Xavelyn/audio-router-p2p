<script lang="ts">
  import { roomStore } from '$lib/stores/rooms.svelte';
  import Network from '@lucide/svelte/icons/network';
  import X from '@lucide/svelte/icons/x';
  import Plus from '@lucide/svelte/icons/plus';

  let { isOpen = $bindable(false) } = $props<{ isOpen: boolean }>();

  let roomId = $state('room-' + Math.floor(100 + Math.random() * 900));
  let roomName = $state('Live Stage Bus');
  let isSubmitting = $state(false);
  let formError = $state<string | null>(null);

  async function handleSubmit(e: SubmitEvent) {
    e.preventDefault();
    if (!roomId.trim() || !roomName.trim()) return;

    isSubmitting = true;
    formError = null;
    try {
      await roomStore.createRoom({
        id: roomId.trim().toLowerCase().replace(/\s+/g, '-'),
        name: roomName.trim(),
      });
      isOpen = false;
      roomId = 'room-' + Math.floor(100 + Math.random() * 900);
      roomName = 'Live Stage Bus';
    } catch (err: any) {
      formError = err.message || 'Failed to create conference room';
    } finally {
      isSubmitting = false;
    }
  }
</script>

{#if isOpen}
  <div class="fixed inset-0 z-50 flex items-center justify-center bg-black/75 backdrop-blur-sm p-4">
    <div class="w-full max-w-md rounded-2xl border border-zinc-800 bg-zinc-950 p-6 shadow-2xl">
      <!-- Modal Header -->
      <div class="flex items-center justify-between pb-4 border-b border-zinc-800">
        <div class="flex items-center gap-2.5">
          <div class="flex h-8 w-8 items-center justify-center rounded-lg bg-cyan-500/10 border border-cyan-500/30 text-cyan-400">
            <Network class="w-4 h-4" />
          </div>
          <div>
            <h2 class="text-sm font-bold text-zinc-100">Create Audio Flow Room</h2>
            <p class="text-[11px] font-mono text-zinc-400">New all-to-all summing bus node</p>
          </div>
        </div>
        <button
          type="button"
          onclick={() => (isOpen = false)}
          class="rounded p-1 text-zinc-400 hover:text-white hover:bg-zinc-800 transition-colors"
          aria-label="Close dialog"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      {#if formError}
        <div class="mt-4 rounded-lg bg-red-950/40 border border-red-500/40 p-2.5 text-xs text-red-300">
          {formError}
        </div>
      {/if}

      <form onsubmit={handleSubmit} class="mt-4 space-y-4">
        <div>
          <label for="room-name" class="block text-xs font-mono font-medium text-zinc-300 mb-1">
            Room Display Name
          </label>
          <input
            id="room-name"
            type="text"
            bind:value={roomName}
            required
            placeholder="e.g. Live Stage Bus"
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-xs text-zinc-100 focus:border-cyan-500 focus:outline-none focus:ring-1 focus:ring-cyan-500 font-mono"
          />
        </div>

        <div>
          <label for="room-id" class="block text-xs font-mono font-medium text-zinc-300 mb-1">
            Unique Room ID
          </label>
          <input
            id="room-id"
            type="text"
            bind:value={roomId}
            required
            placeholder="e.g. room-live-stage"
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-xs text-zinc-100 focus:border-cyan-500 focus:outline-none focus:ring-1 focus:ring-cyan-500 font-mono"
          />
          <p class="mt-1 text-[10px] font-mono text-zinc-500">
            Used by distributed peers across the P2P mesh network.
          </p>
        </div>

        <div class="rounded-lg bg-zinc-900/60 p-3 border border-zinc-800/80 text-[11px] font-mono text-zinc-400 leading-relaxed">
          <p class="text-cyan-300 font-semibold mb-1">Room Routing Rule:</p>
          Once created, any input wire attached to this room will sum and play to every output wire attached to this room.
        </div>

        <div class="flex items-center justify-end gap-2 pt-2 border-t border-zinc-800">
          <button
            type="button"
            onclick={() => (isOpen = false)}
            class="rounded-lg px-3 py-1.5 text-xs font-mono text-zinc-400 hover:text-zinc-200 transition-colors"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={isSubmitting}
            class="inline-flex items-center gap-1.5 rounded-lg bg-cyan-500 px-4 py-2 text-xs font-mono font-bold text-zinc-950 shadow-sm shadow-cyan-500/30 hover:bg-cyan-400 transition-all disabled:opacity-50"
          >
            {#if isSubmitting}
              <span>Creating...</span>
            {:else}
              <Plus class="w-3.5 h-3.5" />
              <span>Provision Room</span>
            {/if}
          </button>
        </div>
      </form>
    </div>
  </div>
{/if}
