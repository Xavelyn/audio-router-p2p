import type { AudioDevice, CreateVirtualDeviceRequest, UpdateDeviceRequest } from '$lib/api/types';
import { apiClient } from '$lib/api/client';
import type { MatrixPort } from './matrix.svelte';

class DeviceStore {
  devices = $state<AudioDevice[]>([]);
  isLoading = $state<boolean>(false);
  error = $state<string | null>(null);

  inputDevices = $derived(this.devices.filter((d) => d.max_input > 0));
  outputDevices = $derived(this.devices.filter((d) => d.max_output > 0));
  virtualDevices = $derived(this.devices.filter((d) => d.is_virtual));
  hardwareDevices = $derived(this.devices.filter((d) => !d.is_virtual));

  // Device-level matrix sources (columns): only devices with max_input > 0
  // Output-only devices (max_input === 0) are strictly excluded.
  matrixSourceDevices = $derived(this.devices.filter((d) => d.max_input > 0));

  // Device-level matrix destinations (rows): only devices with max_output > 0
  // Input-only devices (max_output === 0) are strictly excluded.
  matrixDestinationDevices = $derived(this.devices.filter((d) => d.max_output > 0));

  // Generates flat list of input ports for Matrix Crossbar columns
  matrixInputPorts = $derived.by<MatrixPort[]>(() => {
    const ports: MatrixPort[] = [];
    for (const dev of this.devices) {
      if (dev.max_input > 0) {
        for (let ch = 0; ch < dev.max_input; ch++) {
          const chLabel = dev.max_input === 2 ? (ch === 0 ? 'L' : 'R') : `Ch ${ch + 1}`;
          ports.push({
            id: `local:${dev.id}:${ch}`,
            nodeId: 'local',
            deviceId: dev.id,
            deviceName: dev.name,
            channel: ch,
            channelLabel: chLabel,
            isVirtual: dev.is_virtual,
          });
        }
      }
    }
    return ports;
  });

  // Generates flat list of output ports for Matrix Crossbar rows
  matrixOutputPorts = $derived.by<MatrixPort[]>(() => {
    const ports: MatrixPort[] = [];
    for (const dev of this.devices) {
      if (dev.max_output > 0) {
        for (let ch = 0; ch < dev.max_output; ch++) {
          const chLabel = dev.max_output === 2 ? (ch === 0 ? 'L' : 'R') : `Ch ${ch + 1}`;
          ports.push({
            id: `local:${dev.id}:${ch}`,
            nodeId: 'local',
            deviceId: dev.id,
            deviceName: dev.name,
            channel: ch,
            channelLabel: chLabel,
            isVirtual: dev.is_virtual,
          });
        }
      }
    }
    return ports;
  });

  async loadDevices(kind?: 'all' | 'input' | 'output' | 'virtual') {
    this.isLoading = true;
    this.error = null;
    try {
      const fetched = await apiClient.listDevices(kind);
      this.devices = fetched;
    } catch (err: any) {
      this.error = err.message || 'Failed to enumerate audio devices';
      // If server unreachable, provide sample hardware and loopback devices so UI remains operable
      if (this.devices.length === 0) {
        this.devices = [
          {
            id: 'hw:0',
            name: 'ALSA Default Soundcard (PCM 24-bit/48kHz)',
            max_input: 2,
            max_output: 2,
            default_sample_rate: 48000,
            is_virtual: false,
            volume: 1.0,
            muted: false,
            supported_layouts: ['stereo'],
          },
          {
            id: 'hw:1',
            name: 'USB Audio Interface (Focusrite Scarlett 4i4)',
            max_input: 4,
            max_output: 4,
            default_sample_rate: 96000,
            is_virtual: false,
            volume: 1.0,
            muted: false,
            supported_layouts: ['stereo', 'quad'],
          },
          {
            id: 'virt:loop-main',
            name: 'Virtual Loopback Bus A (DAW Out)',
            max_input: 2,
            max_output: 2,
            default_sample_rate: 48000,
            is_virtual: true,
            volume: 1.0,
            muted: false,
            supported_layouts: ['stereo'],
          },
        ];
      }
    } finally {
      this.isLoading = false;
    }
  }

  async createVirtual(req: CreateVirtualDeviceRequest): Promise<AudioDevice> {
    this.isLoading = true;
    try {
      const created = await apiClient.createVirtualDevice(req);
      this.devices = [...this.devices, created];
      return created;
    } catch (err: any) {
      this.error = err.message || 'Failed to create virtual audio interface';
      throw err;
    } finally {
      this.isLoading = false;
    }
  }

  async deleteVirtual(deviceId: string) {
    this.isLoading = true;
    try {
      await apiClient.deleteVirtualDevice(deviceId);
      this.devices = this.devices.filter((d) => d.id !== deviceId);
    } catch (err: any) {
      this.error = err.message || 'Failed to delete virtual device';
      throw err;
    } finally {
      this.isLoading = false;
    }
  }

  async updateVolume(deviceId: string, volume: number, muted?: boolean) {
    try {
      const updated = await apiClient.updateDevice(deviceId, { volume, muted });
      const idx = this.devices.findIndex((d) => d.id === deviceId);
      if (idx !== -1) {
        this.devices[idx] = updated;
      }
    } catch (err: any) {
      this.error = err.message || 'Failed to update device gain';
    }
  }
}

export const deviceStore = new DeviceStore();
