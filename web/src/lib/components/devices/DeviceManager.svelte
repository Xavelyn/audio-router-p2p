<script lang="ts">
  import { onMount } from 'svelte';
  import { deviceStore } from '$lib/stores/devices.svelte';
  import type { CreateVirtualDeviceRequest } from '$lib/api/types';
  import Speaker from '@lucide/svelte/icons/speaker';
  import Mic from '@lucide/svelte/icons/mic';
  import Plus from '@lucide/svelte/icons/plus';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import Volume2 from '@lucide/svelte/icons/volume-2';
  import VolumeX from '@lucide/svelte/icons/volume-x';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Layers from '@lucide/svelte/icons/layers';
  import X from '@lucide/svelte/icons/x';

  let isModalOpen = $state(false);

  // Form state for creating virtual device
  let newName = $state('Loopback-Bus-A');
  let newDirection = $state<'input' | 'output' | 'duplex'>('duplex');
  let newChannels = $state<1 | 2 | 4 | 6 | 8>(2);
  let newSampleRate = $state<44100 | 48000 | 96000>(48000);
  let isSubmitting = $state(false);
  let formError = $state<string | null>(null);

  onMount(() => {
    deviceStore.loadDevices();
  });

  async function handleCreateVirtual() {
    isSubmitting = true;
    formError = null;
    try {
      await deviceStore.createVirtual({
        name: newName,
        direction: newDirection,
        channels: newChannels,
        sample_rate: newSampleRate,
      });
      isModalOpen = false;
      // Reset form
      newName = 'Loopback-Bus-' + Math.floor(Math.random() * 100);
    } catch (err: any) {
      formError = err.message || 'Failed to create virtual audio interface';
    } finally {
      isSubmitting = false;
    }
  }

  async function handleDeleteVirtual(deviceId: string) {
    if (confirm('Delete this virtual audio loopback interface?')) {
      await deviceStore.deleteVirtual(deviceId);
    }
  }

  function handleVolumeChange(deviceId: string, val: number) {
    deviceStore.updateVolume(deviceId, val);
  }

  function handleMuteToggle(deviceId: string, currentMuted: boolean) {
    deviceStore.updateVolume(deviceId, 1.0, !currentMuted);
  }
</script>

<div class="flex flex-col h-full bg-studio-dark select-none overflow-hidden">
  <!-- Toolbar -->
  <div class="flex items-center justify-between border-b border-zinc-800 bg-zinc-950/70 px-4 py-2.5">
    <div class="flex items-center gap-3">
      <div class="flex items-center gap-2">
        <Speaker class="w-4 h-4 text-cyan-400" />
        <h1 class="text-sm font-semibold tracking-wide text-zinc-100 uppercase">
          Audio Device Registry
        </h1>
      </div>
      <span class="rounded bg-zinc-800 px-2 py-0.5 text-xs font-mono text-zinc-300">
        {deviceStore.devices.length} Detected ({deviceStore.virtualDevices.length} Virtual)
      </span>
    </div>

    <!-- Actions -->
    <div class="flex items-center gap-2">
      <button
        onclick={() => (isModalOpen = true)}
        class="inline-flex items-center gap-1.5 rounded-lg bg-cyan-500 px-3 py-1.5 text-xs font-semibold text-zinc-950 shadow-sm shadow-cyan-500/30 hover:bg-cyan-400 transition-colors"
      >
        <Plus class="w-3.5 h-3.5" />
        <span>Provision Virtual Device</span>
      </button>

      <button
        onclick={() => deviceStore.loadDevices()}
        class="inline-flex items-center justify-center h-7.5 w-7.5 rounded-md border border-zinc-800 bg-zinc-900 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-800 transition-colors"
        title="Refresh Devices"
      >
        <RefreshCw class="w-3.5 h-3.5 {deviceStore.isLoading ? 'animate-spin text-cyan-400' : ''}" />
      </button>
    </div>
  </div>

  <!-- Device Cards Grid -->
  <div class="flex-1 overflow-y-auto p-6 bg-zinc-950">
    <div class="max-w-6xl mx-auto space-y-6">
      <!-- Hardware Section -->
      <div>
        <div class="flex items-center gap-2 mb-3">
          <Speaker class="w-4 h-4 text-zinc-400" />
          <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-300">
            Hardware Soundcards & Host Audio Drivers
          </h2>
        </div>

        <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {#each deviceStore.hardwareDevices as dev}
            <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg hover:border-zinc-700 transition-all flex flex-col justify-between">
              <div>
                <div class="flex items-start justify-between gap-2">
                  <span class="rounded bg-zinc-800/80 px-2 py-0.5 text-[10px] font-mono text-zinc-400 border border-zinc-700/60">
                    ID: {dev.id}
                  </span>
                  <span class="rounded bg-emerald-500/10 px-2 py-0.5 text-[10px] font-mono font-semibold text-emerald-400 border border-emerald-500/20">
                    ALSA/COREAUDIO
                  </span>
                </div>

                <h3 class="mt-2 text-sm font-semibold text-zinc-100 line-clamp-2">
                  {dev.name}
                </h3>

                <!-- Specs Grid -->
                <div class="mt-3 grid grid-cols-2 gap-2 text-xs font-mono">
                  <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                    <span class="text-[10px] text-zinc-400 block">INPUTS</span>
                    <span class="text-zinc-200 font-bold">{dev.max_input} Channels</span>
                  </div>
                  <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                    <span class="text-[10px] text-zinc-400 block">OUTPUTS</span>
                    <span class="text-zinc-200 font-bold">{dev.max_output} Channels</span>
                  </div>
                  <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                    <span class="text-[10px] text-zinc-400 block">SAMPLE RATE</span>
                    <span class="text-cyan-400 font-bold">{dev.default_sample_rate / 1000} kHz</span>
                  </div>
                  <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                    <span class="text-[10px] text-zinc-400 block">LAYOUTS</span>
                    <span class="text-zinc-300 capitalize">{dev.supported_layouts?.join(', ') || 'Stereo'}</span>
                  </div>
                </div>
              </div>

              <!-- Gain & Mute Controls -->
              <div class="mt-4 pt-3 border-t border-zinc-800/80 flex items-center justify-between gap-3">
                <div class="flex items-center gap-2 flex-1">
                  <Volume2 class="w-3.5 h-3.5 text-zinc-400" />
                  <input
                    type="range"
                    min="0"
                    max="1.5"
                    step="0.05"
                    value={dev.volume ?? 1.0}
                    onchange={(e) => handleVolumeChange(dev.id, parseFloat((e.target as HTMLInputElement).value))}
                    class="w-full accent-cyan-400 cursor-pointer h-1.5 bg-zinc-800 rounded-lg"
                  />
                  <span class="text-[10px] font-mono text-zinc-400 w-8">
                    {Math.round((dev.volume ?? 1.0) * 100)}%
                  </span>
                </div>
                <button
                  onclick={() => handleMuteToggle(dev.id, !!dev.muted)}
                  class="rounded p-1 text-zinc-400 hover:text-zinc-100 hover:bg-zinc-800 transition-colors"
                  title="Toggle Mute"
                >
                  {#if dev.muted}
                    <VolumeX class="w-4 h-4 text-red-400" />
                  {:else}
                    <Volume2 class="w-4 h-4 text-zinc-400" />
                  {/if}
                </button>
              </div>
            </div>
          {/each}
        </div>
      </div>

      <!-- Virtual Loopback Devices Section -->
      <div>
        <div class="flex items-center justify-between mb-3">
          <div class="flex items-center gap-2">
            <Layers class="w-4 h-4 text-cyan-400" />
            <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-300">
              Virtual Audio Loopback Interfaces
            </h2>
          </div>
        </div>

        {#if deviceStore.virtualDevices.length === 0}
          <div class="rounded-xl border border-dashed border-zinc-800 p-8 text-center bg-zinc-900/30">
            <p class="text-xs text-zinc-500">No virtual loopback devices provisioned on this host.</p>
            <button
              onclick={() => (isModalOpen = true)}
              class="mt-2 text-xs font-mono text-cyan-400 hover:underline"
            >
              + Provision a new virtual loopback interface
            </button>
          </div>
        {:else}
          <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
            {#each deviceStore.virtualDevices as vDev}
              <div class="rounded-xl border border-cyan-900/40 bg-zinc-900/80 p-4 shadow-lg hover:border-cyan-700/60 transition-all flex flex-col justify-between">
                <div>
                  <div class="flex items-start justify-between gap-2">
                    <span class="rounded bg-cyan-500/10 px-2 py-0.5 text-[10px] font-mono text-cyan-400 border border-cyan-500/20">
                      VIRTUAL BUS
                    </span>
                    <button
                      onclick={() => handleDeleteVirtual(vDev.id)}
                      class="rounded p-1 text-zinc-500 hover:text-red-400 hover:bg-red-950/30 transition-colors"
                      title="Delete Virtual Device"
                    >
                      <Trash2 class="w-3.5 h-3.5" />
                    </button>
                  </div>

                  <h3 class="mt-2 text-sm font-semibold text-zinc-100">
                    {vDev.name}
                  </h3>

                  <div class="mt-3 grid grid-cols-2 gap-2 text-xs font-mono">
                    <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                      <span class="text-[10px] text-zinc-400 block">CHANNELS</span>
                      <span class="text-zinc-200 font-bold">{Math.max(vDev.max_input, vDev.max_output)} Channels</span>
                    </div>
                    <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                      <span class="text-[10px] text-zinc-400 block">SAMPLE RATE</span>
                      <span class="text-cyan-400 font-bold">{vDev.default_sample_rate / 1000} kHz</span>
                    </div>
                  </div>
                </div>

                <!-- Footer volume / action -->
                <div class="mt-4 pt-3 border-t border-zinc-800/80 flex items-center justify-between text-[11px] font-mono text-zinc-400">
                  <span>Latency: &le; 10ms</span>
                  <span class="text-emerald-400">DSP ONLINE</span>
                </div>
              </div>
            {/each}
          </div>
        {/if}
      </div>
    </div>
  </div>
</div>

<!-- Provision Virtual Device Modal -->
{#if isModalOpen}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/80 backdrop-blur-sm p-4"
    role="dialog"
    aria-modal="true"
  >
    <div class="w-full max-w-md rounded-xl border border-zinc-800 bg-zinc-950 p-6 shadow-2xl text-zinc-100">
      <div class="flex items-center justify-between border-b border-zinc-800 pb-3 mb-4">
        <div class="flex items-center gap-2">
          <Layers class="w-4 h-4 text-cyan-400" />
          <h2 class="text-sm font-semibold text-zinc-100 uppercase tracking-wide">
            Provision Virtual Device
          </h2>
        </div>
        <button
          onclick={() => (isModalOpen = false)}
          class="rounded p-1 text-zinc-400 hover:text-zinc-200"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <div class="space-y-4">
        <!-- Name -->
        <div>
          <label for="virt-name" class="block text-xs font-mono text-zinc-300 mb-1">DEVICE NAME</label>
          <input
            id="virt-name"
            type="text"
            bind:value={newName}
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 focus:border-cyan-500 focus:outline-none"
          />
        </div>

        <!-- Direction -->
        <div>
          <label for="virt-direction" class="block text-xs font-mono text-zinc-300 mb-1">DIRECTION</label>
          <select
            id="virt-direction"
            bind:value={newDirection}
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 focus:border-cyan-500 focus:outline-none"
          >
            <option value="duplex">Duplex (Input + Output Sink)</option>
            <option value="input">Input Source Only</option>
            <option value="output">Output Sink Only</option>
          </select>
        </div>

        <!-- Channels & Sample Rate -->
        <div class="grid grid-cols-2 gap-3">
          <div>
            <label for="virt-channels" class="block text-xs font-mono text-zinc-300 mb-1">CHANNELS</label>
            <select
              id="virt-channels"
              bind:value={newChannels}
              class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 focus:border-cyan-500 focus:outline-none"
            >
              <option value={1}>1 (Mono)</option>
              <option value={2}>2 (Stereo)</option>
              <option value={4}>4 (Quad)</option>
              <option value={6}>6 (5.1 Surround)</option>
              <option value={8}>8 (7.1 Surround)</option>
            </select>
          </div>

          <div>
            <label for="virt-sample-rate" class="block text-xs font-mono text-zinc-300 mb-1">SAMPLE RATE</label>
            <select
              id="virt-sample-rate"
              bind:value={newSampleRate}
              class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 focus:border-cyan-500 focus:outline-none"
            >
              <option value={44100}>44.1 kHz</option>
              <option value={48000}>48.0 kHz</option>
              <option value={96000}>96.0 kHz</option>
            </select>
          </div>
        </div>

        {#if formError}
          <div class="text-xs text-red-400 bg-red-950/40 p-2 rounded border border-red-800/50">
            {formError}
          </div>
        {/if}
      </div>

      <div class="mt-6 flex justify-end gap-2 border-t border-zinc-800 pt-4">
        <button
          onclick={() => (isModalOpen = false)}
          class="rounded-lg px-3 py-1.5 text-xs text-zinc-400 hover:text-zinc-200"
        >
          Cancel
        </button>
        <button
          onclick={handleCreateVirtual}
          disabled={isSubmitting}
          class="rounded-lg bg-cyan-500 px-4 py-1.5 text-xs font-semibold text-zinc-950 hover:bg-cyan-400 disabled:opacity-50"
        >
          {isSubmitting ? 'Provisioning...' : 'Provision Interface'}
        </button>
      </div>
    </div>
  </div>
{/if}
