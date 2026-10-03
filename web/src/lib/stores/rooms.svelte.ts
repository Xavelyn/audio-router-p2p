import type { RoomInfo, CreateRoomRequest, UpdateRoomBindingsRequest } from '$lib/api/types';
import { apiClient } from '$lib/api/client';

class RoomStore {
  rooms = $state<RoomInfo[]>([]);
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
      this.rooms = [];
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
      throw err;
    } finally {
      this.isLoading = false;
    }
  }

  async deleteRoom(roomId: string) {
    this.isLoading = true;
    this.lastError = null;
    // Optimistic local removal
    this.rooms = this.rooms.filter((r) => r.id !== roomId);
    try {
      await apiClient.deleteRoom(roomId);
    } catch (err: any) {
      const msg = err.message || 'Failed to delete room';
      this.lastError = msg;
      this.error = msg;
      throw err;
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
    } finally {
      this.isLoading = false;
    }
  }

  async addRoomInput(roomId: string, devId: string, nodeId?: string) {
    this.lastError = null;
    try {
      const updated = await apiClient.addRoomInput(roomId, devId, undefined, nodeId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms = this.rooms.map((r, i) =>
          i === idx
            ? {
                ...r,
                ...updated,
                inputs: updated.inputs || [],
                outputs: updated.outputs || [],
                synced: updated.synced ?? r.synced,
              }
            : r
        );
      }
    } catch (err: any) {
      const msg = err.message || `Failed to assign input ${devId} to room ${roomId}`;
      this.lastError = msg;
      this.error = msg;
    }
  }

  async removeRoomInput(roomId: string, devId: string) {
    this.lastError = null;
    // Optimistic local unbinding
    const idx = this.rooms.findIndex((r) => r.id === roomId);
    if (idx !== -1) {
      this.rooms = this.rooms.map((r, i) =>
        i === idx ? { ...r, inputs: (r.inputs || []).filter((id) => id !== devId && !id.endsWith(`/${devId}`)) } : r
      );
    }
    try {
      const updated = await apiClient.removeRoomInput(roomId, devId);
      const updatedIdx = this.rooms.findIndex((r) => r.id === roomId);
      if (updatedIdx !== -1) {
        this.rooms = this.rooms.map((r, i) =>
          i === updatedIdx
            ? {
                ...r,
                ...updated,
                inputs: updated.inputs || [],
                outputs: updated.outputs || [],
                synced: updated.synced ?? r.synced,
              }
            : r
        );
      }
    } catch (err: any) {
      const msg = err.message || `Failed to remove input ${devId} from room ${roomId}`;
      this.lastError = msg;
      this.error = msg;
    }
  }

  async addRoomOutput(roomId: string, devId: string, nodeId?: string) {
    this.lastError = null;
    try {
      const updated = await apiClient.addRoomOutput(roomId, devId, undefined, nodeId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms = this.rooms.map((r, i) =>
          i === idx
            ? {
                ...r,
                ...updated,
                inputs: updated.inputs || [],
                outputs: updated.outputs || [],
                synced: updated.synced ?? r.synced,
              }
            : r
        );
      }
    } catch (err: any) {
      const msg = err.message || `Failed to assign output ${devId} to room ${roomId}`;
      this.lastError = msg;
      this.error = msg;
    }
  }

  async removeRoomOutput(roomId: string, devId: string) {
    this.lastError = null;
    // Optimistic local unbinding
    const idx = this.rooms.findIndex((r) => r.id === roomId);
    if (idx !== -1) {
      this.rooms = this.rooms.map((r, i) =>
        i === idx ? { ...r, outputs: (r.outputs || []).filter((id) => id !== devId && !id.endsWith(`/${devId}`)) } : r
      );
    }
    try {
      const updated = await apiClient.removeRoomOutput(roomId, devId);
      const updatedIdx = this.rooms.findIndex((r) => r.id === roomId);
      if (updatedIdx !== -1) {
        this.rooms = this.rooms.map((r, i) =>
          i === updatedIdx
            ? {
                ...r,
                ...updated,
                inputs: updated.inputs || [],
                outputs: updated.outputs || [],
                synced: updated.synced ?? r.synced,
              }
            : r
        );
      }
    } catch (err: any) {
      const msg = err.message || `Failed to remove output ${devId} from room ${roomId}`;
      this.lastError = msg;
      this.error = msg;
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
    }
  }
}

export const roomStore = new RoomStore();
