import type { RoomInfo, CreateRoomRequest, UpdateRoomBindingsRequest, LatencyPreset } from '$lib/api/types';
import { apiClient } from '$lib/api/client';

class RoomStore {
  rooms = $state<RoomInfo[]>([]);
  activeJoinedRoomId = $state<string | null>(null);
  isLoading = $state<boolean>(false);
  error = $state<string | null>(null);
  lastError = $state<string | null>(null);

  clearLastError() {
    this.lastError = null;
  }

  async loadRooms() {
    this.isLoading = true;
    this.error = null;
    try {
      const list = await apiClient.listRooms();
      // Ensure inputs, outputs, synced exist even if older API returned partial data
      this.rooms = list.map((r) => ({
        ...r,
        inputs: r.inputs || [],
        outputs: r.outputs || [],
        synced: r.synced ?? false,
      }));
    } catch (err: any) {
      this.error = err.message || 'Failed to enumerate audio conference rooms';
      if (this.rooms.length === 0) {
        this.rooms = [
          {
            id: 'room-main-broadcast',
            name: 'Main Stage Broadcast',
            leader: 'node-studio-ctrl-01',
            state: 'active',
            members: ['node-studio-ctrl-01', 'node-stage-box-02', 'local'],
            inputs: ['hw:0', 'virt:loop-main'],
            outputs: ['hw:0', 'hw:1'],
            synced: true,
          },
          {
            id: 'room-in-ear-monitors',
            name: 'IEM Low-Latency Mix (Musicians)',
            leader: 'node-stage-box-02',
            state: 'active',
            members: ['node-stage-box-02', 'local'],
            inputs: ['hw:1'],
            outputs: ['hw:1'],
            synced: true,
          },
          {
            id: 'room-control-booth',
            name: 'Production Talkback & Intercom',
            leader: 'local',
            state: 'idle',
            members: ['local'],
            inputs: ['virt:loop-main'],
            outputs: ['hw:0'],
            synced: false,
          },
        ];
        this.activeJoinedRoomId = 'room-main-broadcast';
      }
    } finally {
      this.isLoading = false;
    }
  }

  async createRoom(req: CreateRoomRequest): Promise<RoomInfo> {
    this.isLoading = true;
    this.lastError = null;
    try {
      const created = await apiClient.createRoom(req);
      const normalized: RoomInfo = {
        ...created,
        inputs: created.inputs || [],
        outputs: created.outputs || [],
        synced: created.synced ?? false,
      };
      this.rooms = [...this.rooms, normalized];
      return normalized;
    } catch (err: any) {
      const msg = err.message || 'Failed to create audio conference room';
      this.lastError = msg;
      this.error = msg;
      // Optimistic local add if daemon unavailable
      const fallback: RoomInfo = {
        id: req.id,
        name: req.name,
        leader: 'local',
        state: 'active',
        members: ['local'],
        inputs: [],
        outputs: [],
        synced: true,
      };
      this.rooms = [...this.rooms, fallback];
      return fallback;
    } finally {
      this.isLoading = false;
    }
  }

  async deleteRoom(roomId: string) {
    this.isLoading = true;
    this.lastError = null;
    try {
      await apiClient.deleteRoom(roomId);
      this.rooms = this.rooms.filter((r) => r.id !== roomId);
      if (this.activeJoinedRoomId === roomId) {
        this.activeJoinedRoomId = null;
      }
    } catch (err: any) {
      const msg = err.message || 'Failed to delete room';
      this.lastError = msg;
      this.error = msg;
      // Optimistic local removal
      this.rooms = this.rooms.filter((r) => r.id !== roomId);
      if (this.activeJoinedRoomId === roomId) {
        this.activeJoinedRoomId = null;
      }
    } finally {
      this.isLoading = false;
    }
  }

  async joinRoom(roomId: string) {
    this.isLoading = true;
    this.lastError = null;
    try {
      const updated = await apiClient.joinRoom(roomId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = {
          ...this.rooms[idx],
          ...updated,
          inputs: updated.inputs || this.rooms[idx].inputs || [],
          outputs: updated.outputs || this.rooms[idx].outputs || [],
          synced: updated.synced ?? this.rooms[idx].synced,
        };
      }
      this.activeJoinedRoomId = roomId;
    } catch (err: any) {
      const msg = err.message || 'Failed to join room';
      this.lastError = msg;
      this.error = msg;
      // Local fallback for UI preview
      this.activeJoinedRoomId = roomId;
      const target = this.rooms.find((r) => r.id === roomId);
      if (target && target.members && !target.members.includes('local')) {
        target.members.push('local');
      }
    } finally {
      this.isLoading = false;
    }
  }

  async leaveRoom(roomId: string) {
    this.isLoading = true;
    this.lastError = null;
    try {
      const updated = await apiClient.leaveRoom(roomId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = {
          ...this.rooms[idx],
          ...updated,
          inputs: updated.inputs || this.rooms[idx].inputs || [],
          outputs: updated.outputs || this.rooms[idx].outputs || [],
          synced: updated.synced ?? this.rooms[idx].synced,
        };
      }
      if (this.activeJoinedRoomId === roomId) {
        this.activeJoinedRoomId = null;
      }
    } catch (err: any) {
      const msg = err.message || 'Failed to leave room';
      this.lastError = msg;
      this.error = msg;
      if (this.activeJoinedRoomId === roomId) {
        this.activeJoinedRoomId = null;
      }
      const target = this.rooms.find((r) => r.id === roomId);
      if (target && target.members) {
        target.members = target.members.filter((m) => m !== 'local');
      }
    } finally {
      this.isLoading = false;
    }
  }

  async updateRoomBindings(roomId: string, req: UpdateRoomBindingsRequest) {
    this.isLoading = true;
    this.lastError = null;
    try {
      const updated = await apiClient.updateRoomBindings(roomId, req);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = {
          ...this.rooms[idx],
          ...updated,
          inputs: updated.inputs || [],
          outputs: updated.outputs || [],
          synced: updated.synced ?? false,
        };
      }
    } catch (err: any) {
      const msg = err.message || 'Failed to update room bindings';
      this.lastError = msg;
      this.error = msg;
      // Local optimistic update
      const target = this.rooms.find((r) => r.id === roomId);
      if (target) {
        target.inputs = [...req.inputs];
        target.outputs = [...req.outputs];
        if (req.synced !== undefined) {
          target.synced = req.synced;
        }
      }
    } finally {
      this.isLoading = false;
    }
  }

  async addRoomInput(roomId: string, devId: string) {
    this.lastError = null;
    try {
      const updated = await apiClient.addRoomInput(roomId, devId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = {
          ...this.rooms[idx],
          ...updated,
          inputs: updated.inputs || [],
          outputs: updated.outputs || [],
          synced: updated.synced ?? this.rooms[idx].synced,
        };
      }
    } catch (err: any) {
      const msg = err.message || `Failed to assign input ${devId} to room ${roomId}`;
      this.lastError = msg;
      this.error = msg;
      // Local optimistic fallback
      const target = this.rooms.find((r) => r.id === roomId);
      if (target && !target.inputs.includes(devId)) {
        target.inputs.push(devId);
      }
    }
  }

  async removeRoomInput(roomId: string, devId: string) {
    this.lastError = null;
    try {
      const updated = await apiClient.removeRoomInput(roomId, devId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = {
          ...this.rooms[idx],
          ...updated,
          inputs: updated.inputs || [],
          outputs: updated.outputs || [],
          synced: updated.synced ?? this.rooms[idx].synced,
        };
      }
    } catch (err: any) {
      const msg = err.message || `Failed to remove input ${devId} from room ${roomId}`;
      this.lastError = msg;
      this.error = msg;
      // Local optimistic fallback
      const target = this.rooms.find((r) => r.id === roomId);
      if (target) {
        target.inputs = target.inputs.filter((id) => id !== devId);
      }
    }
  }

  async addRoomOutput(roomId: string, devId: string) {
    this.lastError = null;
    try {
      const updated = await apiClient.addRoomOutput(roomId, devId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = {
          ...this.rooms[idx],
          ...updated,
          inputs: updated.inputs || [],
          outputs: updated.outputs || [],
          synced: updated.synced ?? this.rooms[idx].synced,
        };
      }
    } catch (err: any) {
      const msg = err.message || `Failed to assign output ${devId} to room ${roomId}`;
      this.lastError = msg;
      this.error = msg;
      // Local optimistic fallback
      const target = this.rooms.find((r) => r.id === roomId);
      if (target && !target.outputs.includes(devId)) {
        target.outputs.push(devId);
      }
    }
  }

  async removeRoomOutput(roomId: string, devId: string) {
    this.lastError = null;
    try {
      const updated = await apiClient.removeRoomOutput(roomId, devId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = {
          ...this.rooms[idx],
          ...updated,
          inputs: updated.inputs || [],
          outputs: updated.outputs || [],
          synced: updated.synced ?? this.rooms[idx].synced,
        };
      }
    } catch (err: any) {
      const msg = err.message || `Failed to remove output ${devId} from room ${roomId}`;
      this.lastError = msg;
      this.error = msg;
      // Local optimistic fallback
      const target = this.rooms.find((r) => r.id === roomId);
      if (target) {
        target.outputs = target.outputs.filter((id) => id !== devId);
      }
    }
  }

  async setRoomSync(roomId: string, synced: boolean) {
    this.lastError = null;
    try {
      const updated = await apiClient.setRoomSync(roomId, synced);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = {
          ...this.rooms[idx],
          ...updated,
          inputs: updated.inputs || [],
          outputs: updated.outputs || [],
          synced: updated.synced,
        };
      }
    } catch (err: any) {
      const msg = err.message || `Failed to set room sync to ${synced}`;
      this.lastError = msg;
      this.error = msg;
      // Local optimistic fallback
      const target = this.rooms.find((r) => r.id === roomId);
      if (target) {
        target.synced = synced;
      }
    }
  }

  // Select a room's latency preset (low | balanced | reliable). Applied live
  // by the daemon; the response carries the updated room.
  async setRoomLatency(roomId: string, preset: LatencyPreset) {
    this.lastError = null;
    try {
      const updated = await apiClient.setRoomLatency(roomId, preset);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = {
          ...this.rooms[idx],
          ...updated,
          inputs: updated.inputs || this.rooms[idx].inputs || [],
          outputs: updated.outputs || this.rooms[idx].outputs || [],
          synced: updated.synced ?? this.rooms[idx].synced,
        };
      }
    } catch (err: any) {
      const msg = err.message || `Failed to set room latency preset to ${preset}`;
      this.lastError = msg;
      this.error = msg;
    }
  }
}

export const roomStore = new RoomStore();
