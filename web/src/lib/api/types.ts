/**
 * Audio Router P2P - Control Plane API Type Definitions
 * Directly aligned with OpenAPI 3.1.0 specification (api/openapi.yaml)
 */

export interface ErrorResponse {
  error: string;
  code: string;
  details?: Record<string, unknown>;
}

// ============================================================================
// SYSTEM & PEERS
// ============================================================================

export interface SystemStatus {
  status: 'ok' | 'degraded' | 'error';
  version: string;
  uptime_seconds: number;
  active_routes: number;
  discovered_peers: number;
  clock_sync_locked?: boolean;
}

export interface PeerNode {
  id: string;
  addr: string;
  status: 'connected' | 'negotiating' | 'disconnected';
  rtt_ms?: number;
  last_seen?: string;
}

export interface ConnectPeerRequest {
  addr: string;
  tls_fingerprint?: string;
}

export interface ClockSyncStatus {
  locked: boolean;
  offset_ns: number;
  drift_ppm: number;
  source?: 'ptp' | 'ntp' | 'internal';
}

export interface PeerOffset {
  peer_id: string;
  offset_ns: number;
  rtt_ms: number;
  drift_ppm?: number;
  last_sync?: string;
}

export interface NetworkInterface {
  name: string;
  ip: string;
  mac?: string;
  mtu?: number;
  is_loopback?: boolean;
  is_up?: boolean;
}

// ============================================================================
// AUDIO DEVICES
// ============================================================================

export type ChannelLayout = 'mono' | 'stereo' | 'quad' | '5.1' | '7.1' | 'custom';

export interface AudioDevice {
  id: string;
  urn?: string;
  legacy_id?: string;
  name: string;
  max_input: number;
  max_output: number;
  default_sample_rate: number;
  is_virtual: boolean;
  status?: 'active' | 'quarantined' | 'disconnected' | string;
  volume?: number;
  muted?: boolean;
  supported_layouts?: ChannelLayout[];
}

export interface CreateVirtualDeviceRequest {
  name: string;
  direction: 'input' | 'output' | 'duplex';
  channels: 1 | 2 | 4 | 6 | 8;
  sample_rate: 44100 | 48000 | 96000;
  buffer_size_frames?: number;
}

export type VirtualDeviceCreateRequest = CreateVirtualDeviceRequest;

export interface UpdateDeviceRequest {
  volume?: number;
  muted?: boolean;
}

export type DeviceConfigUpdate = UpdateDeviceRequest;

// ============================================================================
// MATRIX ROUTING
// ============================================================================

export interface MatrixEndpoint {
  node_id: string;
  device_id: string;
  layout?: ChannelLayout;
  channels: number[];
}

export type RouteSource = MatrixEndpoint;
export type RouteDestination = MatrixEndpoint;

export interface MatrixRoute {
  id: string;
  name?: string;
  source: MatrixEndpoint;
  destination: MatrixEndpoint;
  in_channels: number;
  out_channels: number;
  /**
   * Row-major flat gain matrix of size out_channels * in_channels.
   * Linear gain multiplier [0.0 .. 2.0] (-inf to +6 dB).
   * Index: coefficients[out_ch * in_channels + in_ch]
   */
  coefficients: number[];
  slew_ms: number;
  muted?: boolean;
  state: 'active' | 'slewing' | 'paused' | 'error';
}

export interface CreateRouteRequest {
  id?: string;
  name?: string;
  source: MatrixEndpoint;
  destination: MatrixEndpoint;
  coefficients?: number[];
  slew_ms?: number;
}

export interface PatchRouteRequest {
  name?: string;
  coefficients?: number[];
  slew_ms?: number;
  muted?: boolean;
}

export interface CrosspointGainRequest {
  out_channel: number;
  in_channel: number;
  gain: number;
  slew_ms?: number;
}

export type LiveGainUpdate = CrosspointGainRequest;

export interface GainMatrix {
  in_channels: number;
  out_channels: number;
  coefficients: number[];
}

// ============================================================================
// ROOM COORDINATION
// ============================================================================

export interface RoomInfo {
  id: string;
  name: string;
  leader?: string;
  state: 'idle' | 'active' | 'degraded';
  members?: string[];
  inputs: string[];
  outputs: string[];
  synced: boolean;
  input_gains?: Record<string, number>;
  output_gains?: Record<string, number>;
}

export type Room = RoomInfo;
export type RoomDetails = RoomInfo;

export interface RoomMember {
  id: string;
  name?: string;
  addr?: string;
  is_leader: boolean;
  offset_ns?: number;
}

export interface CreateRoomRequest {
  id: string;
  name: string;
}

export type RoomCreateRequest = CreateRoomRequest;

export interface UpdateRoomBindingsRequest {
  inputs: string[];
  outputs: string[];
  synced?: boolean;
}

export interface RoomDeviceBindingRequest {
  device_id: string;
  gain?: number;
  muted?: boolean;
}

export interface SetRoomSyncRequest {
  synced: boolean;
}

// ============================================================================
// TELEMETRY & DIAGNOSTICS
// ============================================================================

export interface ChannelMeter {
  channel: number;
  rms_dbfs: number;
  peak_dbfs: number;
  clipped: boolean;
}

export type DTXState = 'Active' | 'Hangover' | 'Suppressed';

export interface MeterFrame {
  stream_id: string;
  timestamp_ns: number;
  dtx_state: DTXState;
  channels: ChannelMeter[];
}

export type StreamMeterFrame = MeterFrame;

export interface JitterStats {
  current_depth_frames: number;
  target_depth_frames: number;
  underruns: number;
  overruns: number;
  jitter_ms?: number;
}

export interface FECStats {
  group_size_k: number;
  parity_packets_sent_total: number;
  parity_packets_received_total: number;
  packets_recovered_total: number;
  unrecoverable_groups_total: number;
}

export interface PLCStats {
  concealed_frames_total: number;
  pitch_detected_frames_total: number;
  noise_extrapolated_frames_total: number;
  silence_decay_events_total: number;
}

export interface ASRCStats {
  nominal_rate_hz: number;
  smoothed_watermark_frames?: number;
  target_watermark_frames?: number;
  drift_ppm: number;
  resample_ratio: number;
  samples_adjusted_total: number;
}

export interface StreamStats {
  stream_id: string;
  packets_sent: number;
  packets_received: number;
  underruns: number;
  overruns: number;
  late_packets?: number;
  early_packets?: number;
  dtx_state?: DTXState;
  dtx_packets_suppressed_total?: number;
  rms_dbfs?: number;
  peak_dbfs?: number;
  fec: FECStats;
  plc: PLCStats;
  asrc: ASRCStats;
  peers?: Record<string, JitterStats>;
}

export type StreamTelemetry = StreamStats;

// System resource metrics snapshot for diagnostics
export interface SystemMetrics {
  cpu_percent: number;
  memory_used_mb: number;
  memory_total_mb: number;
  goroutines: number;
  gc_pause_ns: number;
}

// ============================================================================
// CLUSTER OBSERVABILITY & NETWORK HEALTH (ADR 0010 & ADR 0011)
// ============================================================================

export type NodeHealthGrade = 'EXCELLENT' | 'GOOD' | 'FAIR' | 'POOR' | 'CRITICAL';

export interface ClockSyncHealth {
  offset_us: number;
  drift_ppm: number;
  sync_rtt_us: number;
  packets_sent: number;
  packets_lost: number;
  is_master: boolean;
}

export interface DiscoveryHealth {
  active_members: number;
  suspect_members: number;
  dead_members: number;
  lan_beacon_active: boolean;
  beacons_sent: number;
  beacons_received: number;
  token_rejections: number;
  seed_retry_active: boolean;
  seed_join_attempts: number;
  seed_join_successes: number;
}

export interface StreamLinkStats {
  stream_id: string;
  peer_id?: string;
  packets_sent: number;
  packets_received: number;
  packets_lost: number;
  loss_ratio: number;
  effective_kbps: number;
  jitter_depth_frames: number;
  jitter_target_delay_ms: number;
  underruns: number;
  overruns: number;
  fec_recovered: number;
  plc_concealed: number;
}

export interface PeerLinkHealth {
  peer_id: string;
  address: string;
  health_score: number;
  health_grade: NodeHealthGrade | string;
  swim_state: 'alive' | 'suspect' | 'dead' | string;
  direct_ping_failed: boolean;
  rtt_ms: number;
  last_seen: string;
  loss_ratio: number;
  bitrate_kbps: number;
  streaming?: StreamLinkStats;
  sync?: ClockSyncHealth;
}

export interface NetworkHealthResponse {
  node_id: string;
  timestamp: string;
  health_score: number;
  health_grade: NodeHealthGrade | string;
  cluster: DiscoveryHealth;
  sync: ClockSyncHealth;
  latency_matrix: Record<string, number>;
  peers: Record<string, PeerLinkHealth>;
  streams?: Record<string, StreamLinkStats>;
  local_underruns: number;
}

export interface AudioDeviceSummary {
  id: string;
  urn?: string;
  legacy_id?: string;
  device_id?: string;
  node_id: string;
  node_name?: string;
  name: string;
  direction: 'input' | 'output' | 'duplex' | string;
  max_input: number;
  max_output: number;
  default_sample_rate: number;
  is_virtual: boolean;
  volume: number;
  muted: boolean;
  supported_layouts?: string[];
  state?: string;
  status?: string;
}

export type ClusterDeviceSummary = AudioDeviceSummary;

export interface ClusterNodeSummary {
  id: string;
  node_id?: string;
  name: string;
  api_addr: string;
  is_local: boolean;
  swim_state: 'alive' | 'suspect' | 'dead' | string;
  health_score: number;
  health_grade: NodeHealthGrade | string;
  clock_sync: ClockSyncHealth;
  clock_role?: 'master' | 'slave' | string;
  clock_offset_us?: number;
  clock_drift_ppm?: number;
  devices: AudioDeviceSummary[];
  active_streams: number;
  uptime_seconds?: number;
  last_seen?: string;
  error_reason?: string;
  packet_loss_ratio?: number;
  rtt_ms?: number;
  bitrate_kbps?: number;
}

export interface ClusterNodesResponse {
  cluster_id?: string;
  total_nodes: number;
  healthy_nodes: number;
  degraded_nodes: number;
  active_nodes?: number;
  timestamp: string;
  nodes: ClusterNodeSummary[];
  nodes_map?: Record<string, ClusterNodeSummary>;
  latency_matrix?: Record<string, Record<string, number>>;
}

export interface ClusterDevicesResponse {
  timestamp: string;
  devices: AudioDeviceSummary[];
}

export interface DeviceMutationRequest {
  volume?: number;
  muted?: boolean;
}

export interface DeviceMutationResponse {
  id?: string;
  device_id: string;
  node_id: string;
  volume: number;
  muted: boolean;
  updated_at: string;
}

// ============================================================================
// HELPER CONVERSIONS
// ============================================================================

export function linearToDb(linear: number): number {
  if (linear <= 0.00001) return -Infinity;
  return 20 * Math.log10(linear);
}

export function dbToLinear(db: number): number {
  if (db <= -96) return 0;
  return Math.min(2.0, Math.max(0, Math.pow(10, db / 20)));
}

export function formatDb(db: number, decimals: number = 1): string {
  if (db === -Infinity || db <= -96) return '-inf dB';
  const sign = db > 0 ? '+' : '';
  return `${sign}${db.toFixed(decimals)} dB`;
}
