import type { RoomInfo, CreateRoomRequest } from '$lib/api/types';
import { apiClient } from '$lib/api/client';

class RoomStore {
  rooms = $state<RoomInfo[]>([]);
  activeJoinedRoomId = $state<string | null>(null);
  isLoading = $state<boolean>(false);
  error = $state<string | null>(null);

  async loadRooms() {
    this.isLoading = true;
    this.error = null;
    try {
      const list = await apiClient.listRooms();
      this.rooms = list;
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
          },
          {
            id: 'room-in-ear-monitors',
            name: 'IEM Low-Latency Mix (Musicians)',
            leader: 'node-stage-box-02',
            state: 'active',
            members: ['node-stage-box-02', 'local'],
          },
          {
            id: 'room-control-booth',
            name: 'Production Talkback & Intercom',
            leader: 'local',
            state: 'idle',
            members: ['local'],
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
    try {
      const created = await apiClient.createRoom(req);
      this.rooms = [...this.rooms, created];
      return created;
    } catch (err: any) {
      this.error = err.message || 'Failed to create audio conference room';
      throw err;
    } finally {
      this.isLoading = false;
    }
  }

  async deleteRoom(roomId: string) {
    this.isLoading = true;
    try {
      await apiClient.deleteRoom(roomId);
      this.rooms = this.rooms.filter((r) => r.id !== roomId);
      if (this.activeJoinedRoomId === roomId) {
        this.activeJoinedRoomId = null;
      }
    } catch (err: any) {
      this.error = err.message || 'Failed to delete room';
      throw err;
    } finally {
      this.isLoading = false;
    }
  }

  async joinRoom(roomId: string) {
    this.isLoading = true;
    try {
      const updated = await apiClient.joinRoom(roomId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = updated;
      }
      this.activeJoinedRoomId = roomId;
    } catch (err: any) {
      this.error = err.message || 'Failed to join room';
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
    try {
      const updated = await apiClient.leaveRoom(roomId);
      const idx = this.rooms.findIndex((r) => r.id === roomId);
      if (idx !== -1) {
        this.rooms[idx] = updated;
      }
      if (this.activeJoinedRoomId === roomId) {
        this.activeJoinedRoomId = null;
      }
    } catch (err: any) {
      this.error = err.message || 'Failed to leave room';
      // Local fallback
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
}

export const roomStore = new RoomStore();
