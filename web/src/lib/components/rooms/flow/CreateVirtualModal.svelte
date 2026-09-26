<script lang="ts">
  import { deviceStore } from '$lib/stores/devices.svelte';
  import Radio from '@lucide/svelte/icons/radio';
  import X from '@lucide/svelte/icons/x';
  import Plus from '@lucide/svelte/icons/plus';

  let { isOpen = $bindable(false) } = $props<{ isOpen: boolean }>();

  let name = $state('Loopback-Bus-' + Math.floor(10 + Math.random() * 90));
  let direction = $state<'input' | 'output' | 'duplex'>('duplex');
  let channels = $state<1 | 2 | 4 | 6 | 8>(2);
  let sampleRate = $state<44100 | 48000 | 96000>(48000);
  let isSubmitting = $state(false);
  let formError = $state<string | null>(null);

  async function handleSubmit(e: SubmitEvent) {
    e.preventDefault();
    if (!name.trim()) return;

    isSubmitting = true;
    formError = null;
    try {
      await deviceStore.createVirtual({
        name: name.trim(),
        direction,
        channels,
        sample_rate: sampleRate,
      });
      isOpen = false;
      name = 'Loopback-Bus-' + Math.floor(10 + Math.random() * 90);
    } catch (err: any) {
      formError = err.message || 'Failed to create virtual audio interface';
    } finally {
      isSubmitting = false;
    }
  }
</script>

{#if isOpen}
  <div class="fixed inset-0 z-50 flex items-center justify-center bg-black/75 backdrop-blur-sm p-4">
    <div class="w-full max-w-md rounded-2xl border border-zinc-800 bg-zinc-950 p-6 shadow-2xl">
      <div class="flex items-center justify-between pb-4 border-b border-zinc-800">
        <div class="flex items-center gap-2.5">
          <div class="flex h-8 w-8 items-center justify-center rounded-lg bg-purple-500/10 border border-purple-500/30 text-purple-400">
            <Radio class="w-4 h-4" />
          </div>
          <div>
            <h2 class="text-sm font-bold text-zinc-100">Create Virtual Audio Loopback</h2>
            <p class="text-[11px] font-mono text-zinc-400">Software sink/source endpoint</p>
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
          <label for="virt-name" class="block text-xs font-mono font-medium text-zinc-300 mb-1">
            Virtual Device Name
          </label>
          <input
            id="virt-name"
            type="text"
            bind:value={name}
            required
            placeholder="e.g. Loopback-DAW-Master"
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-xs text-zinc-100 focus:border-cyan-500 focus:outline-none focus:ring-1 focus:ring-cyan-500 font-mono"
          />
        </div>

        <div class="grid grid-cols-2 gap-3">
          <div>
            <label for="virt-dir" class="block text-xs font-mono font-medium text-zinc-300 mb-1">
              Audio Direction
            </label>
            <select
              id="virt-dir"
              bind:value={direction}
              class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-xs text-zinc-100 focus:border-cyan-500 focus:outline-none font-mono"
            >
              <option value="duplex">Duplex (In & Out)</option>
              <option value="input">Input Only</option>
              <option value="output">Output Only</option>
            </select>
          </div>

          <div>
            <label for="virt-ch" class="block text-xs font-mono font-medium text-zinc-300 mb-1">
              Channels
            </label>
            <select
              id="virt-ch"
              bind:value={channels}
              class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-xs text-zinc-100 focus:border-cyan-500 focus:outline-none font-mono"
            >
              <option value={1}>1 (Mono)</option>
              <option value={2}>2 (Stereo)</option>
              <option value={4}>4 (Quad)</option>
              <option value={6}>6 (5.1)</option>
              <option value={8}>8 (7.1)</option>
            </select>
          </div>
        </div>

        <div>
          <label for="virt-sr" class="block text-xs font-mono font-medium text-zinc-300 mb-1">
            Sample Rate
          </label>
          <select
            id="virt-sr"
            bind:value={sampleRate}
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-xs text-zinc-100 focus:border-cyan-500 focus:outline-none font-mono"
          >
            <option value={44100}>44.1 kHz (CD Audio)</option>
            <option value={48000}>48.0 kHz (Broadcast Standard)</option>
            <option value={96000}>96.0 kHz (High Resolution)</option>
          </select>
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
            class="inline-flex items-center gap-1.5 rounded-lg bg-purple-500 px-4 py-2 text-xs font-mono font-bold text-zinc-950 shadow-sm shadow-purple-500/30 hover:bg-purple-400 transition-all disabled:opacity-50"
          >
            {#if isSubmitting}
              <span>Creating...</span>
            {:else}
              <Plus class="w-3.5 h-3.5" />
              <span>Create Virtual Bus</span>
            {/if}
          </button>
        </div>
      </form>
    </div>
  </div>
{/if}
