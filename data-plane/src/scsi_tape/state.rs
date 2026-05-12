use std::collections::{BTreeMap, VecDeque};
use std::fs::{self, OpenOptions};
use std::io::{self, Write};
use std::path::Path;
use std::sync::OnceLock;

use crate::storage::LayoutPaths;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MountState {
    Empty,
    Loaded,
    Busy,
    Error,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BlockMode {
    Variable,
    Fixed,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BlockModeProfile {
    pub mode: BlockMode,
    pub fixed_block_size: u32,
}

impl Default for BlockModeProfile {
    fn default() -> Self {
        Self {
            // Legacy compatibility baseline:
            // expose a deterministic default fixed block size to host stacks.
            mode: BlockMode::Fixed,
            fixed_block_size: 256 * 1024,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct CommandCounters {
    pub write_ops: u64,
    pub read_ops: u64,
    pub filemark_ops: u64,
    pub space_ops: u64,
    pub mode_sense_ops: u64,
    pub log_sense_ops: u64,
    pub unsupported_mode_pages: u64,
    pub unsupported_log_pages: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct CartridgeUsageCounters {
    pub load_count: u64,
    pub volume_change_reference: u64,
    pub lifetime_write_ops: u64,
    pub lifetime_read_ops: u64,
    pub lifetime_filemark_ops: u64,
    pub lifetime_bytes_written: u64,
    pub lifetime_bytes_read: u64,
    pub last_load_write_ops: u64,
    pub last_load_read_ops: u64,
    pub last_load_filemark_ops: u64,
    pub last_load_bytes_written: u64,
    pub last_load_bytes_read: u64,
}

impl CartridgeUsageCounters {
    fn reset_last_load(&mut self) {
        self.last_load_write_ops = 0;
        self.last_load_read_ops = 0;
        self.last_load_filemark_ops = 0;
        self.last_load_bytes_written = 0;
        self.last_load_bytes_read = 0;
    }

    fn load_from_path(path: &Path) -> io::Result<Self> {
        let raw = match fs::read_to_string(path) {
            Ok(content) => content,
            Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(Self::default()),
            Err(err) => return Err(err),
        };
        let mut counters = Self::default();
        for line in raw.lines() {
            let Some((key, value)) = line.split_once('=') else {
                continue;
            };
            let Ok(parsed) = value.trim().parse::<u64>() else {
                continue;
            };
            match key.trim() {
                "load_count" => counters.load_count = parsed,
                "volume_change_reference" => counters.volume_change_reference = parsed,
                "lifetime_write_ops" => counters.lifetime_write_ops = parsed,
                "lifetime_read_ops" => counters.lifetime_read_ops = parsed,
                "lifetime_filemark_ops" => counters.lifetime_filemark_ops = parsed,
                "lifetime_bytes_written" => counters.lifetime_bytes_written = parsed,
                "lifetime_bytes_read" => counters.lifetime_bytes_read = parsed,
                _ => {}
            }
        }
        Ok(counters)
    }

    fn persist_to_path(&self, path: &Path) -> io::Result<()> {
        let parent = path.parent().ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                "usage counter path parent missing",
            )
        })?;
        fs::create_dir_all(parent)?;
        let body = format!(
            concat!(
                "load_count={}\n",
                "volume_change_reference={}\n",
                "lifetime_write_ops={}\n",
                "lifetime_read_ops={}\n",
                "lifetime_filemark_ops={}\n",
                "lifetime_bytes_written={}\n",
                "lifetime_bytes_read={}\n"
            ),
            self.load_count,
            self.volume_change_reference,
            self.lifetime_write_ops,
            self.lifetime_read_ops,
            self.lifetime_filemark_ops,
            self.lifetime_bytes_written,
            self.lifetime_bytes_read
        );
        let tmp = tmp_path_for(path);
        let mut file = OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(&tmp)?;
        file.write_all(body.as_bytes())?;
        file.sync_all()?;
        drop(file);
        fs::rename(&tmp, path)?;
        let dir = OpenOptions::new().read(true).open(parent)?;
        dir.sync_all()?;
        Ok(())
    }
}

fn tmp_path_for(path: &Path) -> std::path::PathBuf {
    let mut tmp = path.as_os_str().to_os_string();
    tmp.push(".tmp");
    std::path::PathBuf::from(tmp)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AllowOverwriteState {
    pub allow_type: u8,
    pub partition: u8,
    pub block_address: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PartitionRuntime {
    pub partition_units: u8,
    pub addl_partitions_defined: u8,
    pub fdp: u8,
    pub medium_fmt_recognition: u8,
    pub partition_size_units: [u16; 4],
    pub partition_sizes_bytes: [u64; 4],
    pub active_partition: u8,
}

impl Default for PartitionRuntime {
    fn default() -> Self {
        Self {
            partition_units: 9,
            addl_partitions_defined: 0,
            fdp: 0x9C,
            medium_fmt_recognition: 0x03,
            partition_size_units: [0; 4],
            partition_sizes_bytes: [0; 4],
            active_partition: 0,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MamAttributeOverride {
    pub format: u8,
    pub value: Vec<u8>,
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RetentionPolicy {
    pub is_worm_media: bool,
    pub retention_locked: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ReservationState {
    pub generation: u32,
    pub registrations: BTreeMap<String, u64>,
    pub active_owner: Option<String>,
    pub active_key: Option<u64>,
}

#[derive(Debug, Clone)]
pub struct TapeState {
    pub drive_id: String,
    pub cartridge_id: Option<String>,
    pub mount_state: MountState,
    pub current_position: u64,
    pub eod_position: u64,
    pub filemarks: Vec<u64>,
    pub block_starts: Vec<u64>,
    pub block_lengths: BTreeMap<u64, u32>,
    pub active_layout: Option<LayoutPaths>,
    pub early_warning_window: u64,
    pub block_mode: BlockModeProfile,
    pub buffered_mode: bool,
    pub data_compression_allowed: bool,
    pub data_compression_enabled: bool,
    pub select_data_compression_algorithm: bool,
    pub command_counters: CommandCounters,
    pub retention_policy: RetentionPolicy,
    pub reservation_state: ReservationState,
    pub changer_slots: BTreeMap<u16, Option<String>>,
    pub changer_drives: BTreeMap<u16, Option<String>>,
    pub changer_ie_ports: BTreeMap<u16, Option<String>>,
    pub changer_ie_impexp: BTreeMap<u16, bool>,
    pub changer_drive_sources: BTreeMap<u16, Option<u16>>,
    pub changer_slots_synced_from_shared: bool,
    pub partition_runtime: PartitionRuntime,
    pub cartridge_capacity_bytes: Option<u64>,
    pub mam_overrides: BTreeMap<u32, MamAttributeOverride>,
    pub mam_loaded_for: Option<String>,
    pub usage_counters: CartridgeUsageCounters,
    pub usage_counters_dirty_ops: u32,
    pub allow_overwrite: Option<AllowOverwriteState>,
    /// Deferred UNIT ATTENTION queue reported on REQUEST SENSE.
    pub pending_unit_attention: VecDeque<(u8, u8)>,
    pub nexus_unit_attention: BTreeMap<String, VecDeque<(u8, u8)>>,
}

const USAGE_COUNTER_PERSIST_EVERY_OPS: u32 = 1024;

fn usage_counter_persist_every_ops() -> u32 {
    static VALUE: OnceLock<u32> = OnceLock::new();
    *VALUE.get_or_init(|| {
        std::env::var("HOLO_USAGE_COUNTER_PERSIST_EVERY_OPS")
            .ok()
            .and_then(|raw| raw.trim().parse::<u32>().ok())
            .filter(|value| (1..=1_000_000).contains(value))
            .unwrap_or(USAGE_COUNTER_PERSIST_EVERY_OPS)
    })
}

fn initial_data_compression_enabled() -> bool {
    static VALUE: OnceLock<bool> = OnceLock::new();
    *VALUE.get_or_init(|| {
        std::env::var("HOLO_TAPE_COMPRESSION_ENABLED")
            .ok()
            .map(|raw| {
                matches!(
                    raw.trim().to_ascii_lowercase().as_str(),
                    "1" | "true" | "yes" | "on"
                )
            })
            .unwrap_or(true)
    })
}

impl TapeState {
    pub fn new(drive_id: impl Into<String>) -> Self {
        Self {
            drive_id: drive_id.into(),
            cartridge_id: None,
            mount_state: MountState::Empty,
            current_position: 0,
            eod_position: 0,
            filemarks: Vec::new(),
            block_starts: Vec::new(),
            block_lengths: BTreeMap::new(),
            active_layout: None,
            early_warning_window: 8,
            block_mode: BlockModeProfile::default(),
            buffered_mode: true,
            data_compression_allowed: initial_data_compression_enabled(),
            data_compression_enabled: initial_data_compression_enabled(),
            select_data_compression_algorithm: true,
            command_counters: CommandCounters::default(),
            retention_policy: RetentionPolicy::default(),
            reservation_state: ReservationState::default(),
            changer_slots: default_changer_slots(),
            changer_drives: default_changer_drives(),
            changer_ie_ports: default_changer_ie_ports(),
            changer_ie_impexp: default_changer_ie_impexp(),
            changer_drive_sources: default_changer_drive_sources(),
            changer_slots_synced_from_shared: false,
            partition_runtime: PartitionRuntime::default(),
            cartridge_capacity_bytes: None,
            mam_overrides: BTreeMap::new(),
            mam_loaded_for: None,
            usage_counters: CartridgeUsageCounters::default(),
            usage_counters_dirty_ops: 0,
            allow_overwrite: None,
            pending_unit_attention: initial_unit_attention_queue(),
            nexus_unit_attention: BTreeMap::new(),
        }
    }

    pub fn mount(&mut self, cartridge_id: impl Into<String>, layout: LayoutPaths) {
        self.cartridge_id = Some(cartridge_id.into());
        self.mount_state = MountState::Loaded;
        self.current_position = 0;
        self.eod_position = 0;
        self.filemarks.clear();
        self.block_starts.clear();
        self.block_lengths.clear();
        self.active_layout = Some(layout);
        self.early_warning_window = 8;
        self.block_mode = BlockModeProfile::default();
        self.buffered_mode = true;
        self.command_counters = CommandCounters::default();
        self.retention_policy = RetentionPolicy::default();
        self.reservation_state = ReservationState::default();
        self.partition_runtime = PartitionRuntime::default();
        self.cartridge_capacity_bytes = None;
        self.mam_overrides.clear();
        self.mam_loaded_for = None;
        self.allow_overwrite = None;
        self.usage_counters = self
            .usage_counter_path()
            .and_then(|path| CartridgeUsageCounters::load_from_path(&path).ok())
            .unwrap_or_default();
        self.usage_counters.load_count = self.usage_counters.load_count.saturating_add(1);
        self.usage_counters.volume_change_reference = self
            .usage_counters
            .volume_change_reference
            .saturating_add(1);
        self.usage_counters.reset_last_load();
        self.usage_counters_dirty_ops = 0;
        if let Err(err) = self.persist_usage_counters() {
            eprintln!("[tcmu_handler] persist usage counters on mount failed: {err}");
        }
        self.pending_unit_attention.clear();
        self.nexus_unit_attention.clear();
        self.push_unit_attention(0x28, 0x00);
    }

    pub fn unmount(&mut self) {
        if let Err(err) = self.persist_usage_counters() {
            eprintln!("[tcmu_handler] persist usage counters on unmount failed: {err}");
        }
        self.cartridge_id = None;
        self.mount_state = MountState::Empty;
        self.current_position = 0;
        self.eod_position = 0;
        self.filemarks.clear();
        self.block_starts.clear();
        self.block_lengths.clear();
        self.active_layout = None;
        self.early_warning_window = 8;
        self.block_mode = BlockModeProfile::default();
        self.buffered_mode = true;
        self.command_counters = CommandCounters::default();
        self.retention_policy = RetentionPolicy::default();
        self.reservation_state = ReservationState::default();
        self.partition_runtime = PartitionRuntime::default();
        self.cartridge_capacity_bytes = None;
        self.mam_overrides.clear();
        self.mam_loaded_for = None;
        self.usage_counters = CartridgeUsageCounters::default();
        self.usage_counters_dirty_ops = 0;
        self.allow_overwrite = None;
        self.pending_unit_attention.clear();
        self.nexus_unit_attention.clear();
    }

    pub fn usage_counter_path(&self) -> Option<std::path::PathBuf> {
        self.active_layout
            .as_ref()
            .map(|layout| layout.usage_counters_file())
    }

    pub fn persist_usage_counters(&mut self) -> io::Result<()> {
        let Some(path) = self.usage_counter_path() else {
            return Ok(());
        };
        self.usage_counters.persist_to_path(&path)?;
        self.usage_counters_dirty_ops = 0;
        Ok(())
    }

    fn mark_usage_counters_dirty(&mut self) -> io::Result<()> {
        self.usage_counters_dirty_ops = self.usage_counters_dirty_ops.saturating_add(1);
        if self.usage_counters_dirty_ops >= usage_counter_persist_every_ops() {
            return self.persist_usage_counters();
        }
        Ok(())
    }

    pub fn record_write_usage(&mut self, bytes: u64) -> io::Result<()> {
        self.usage_counters.lifetime_write_ops =
            self.usage_counters.lifetime_write_ops.saturating_add(1);
        self.usage_counters.last_load_write_ops =
            self.usage_counters.last_load_write_ops.saturating_add(1);
        self.usage_counters.lifetime_bytes_written = self
            .usage_counters
            .lifetime_bytes_written
            .saturating_add(bytes);
        self.usage_counters.last_load_bytes_written = self
            .usage_counters
            .last_load_bytes_written
            .saturating_add(bytes);
        self.mark_usage_counters_dirty()
    }

    pub fn record_read_usage(&mut self, bytes: u64) -> io::Result<()> {
        self.usage_counters.lifetime_read_ops =
            self.usage_counters.lifetime_read_ops.saturating_add(1);
        self.usage_counters.last_load_read_ops =
            self.usage_counters.last_load_read_ops.saturating_add(1);
        self.usage_counters.lifetime_bytes_read = self
            .usage_counters
            .lifetime_bytes_read
            .saturating_add(bytes);
        self.usage_counters.last_load_bytes_read = self
            .usage_counters
            .last_load_bytes_read
            .saturating_add(bytes);
        self.mark_usage_counters_dirty()
    }

    pub fn record_filemark_usage(&mut self, count: u64) -> io::Result<()> {
        self.usage_counters.lifetime_filemark_ops = self
            .usage_counters
            .lifetime_filemark_ops
            .saturating_add(count);
        self.usage_counters.last_load_filemark_ops = self
            .usage_counters
            .last_load_filemark_ops
            .saturating_add(count);
        self.mark_usage_counters_dirty()
    }

    pub fn reset_changer_inventory(&mut self) {
        self.changer_slots = default_changer_slots();
        self.changer_drives = default_changer_drives();
        self.changer_ie_ports = default_changer_ie_ports();
        self.changer_ie_impexp = default_changer_ie_impexp();
        self.changer_drive_sources = default_changer_drive_sources();
        self.changer_slots_synced_from_shared = false;
    }

    pub fn push_unit_attention(&mut self, asc: u8, ascq: u8) {
        if self.pending_unit_attention.len() >= 16 {
            let _ = self.pending_unit_attention.pop_front();
        }
        self.pending_unit_attention.push_back((asc, ascq));
    }

    pub fn take_unit_attention(&mut self) -> Option<(u8, u8)> {
        self.pending_unit_attention.pop_front()
    }

    pub fn take_nexus_unit_attention(&mut self, initiator: &str) -> VecDeque<(u8, u8)> {
        let key = normalize_nexus_key(initiator);
        let fallback = initial_unit_attention_queue();
        std::mem::replace(
            &mut self.pending_unit_attention,
            self.nexus_unit_attention.remove(&key).unwrap_or(fallback),
        )
    }

    pub fn restore_nexus_unit_attention(
        &mut self,
        initiator: &str,
        default_queue: VecDeque<(u8, u8)>,
    ) {
        let key = normalize_nexus_key(initiator);
        let nexus_queue = std::mem::replace(&mut self.pending_unit_attention, default_queue);
        self.nexus_unit_attention.insert(key, nexus_queue);
    }

    pub fn changer_slot_count(&self) -> u16 {
        let count = self.changer_slots.len();
        if count == 0 {
            return 1;
        }
        u16::try_from(count).unwrap_or(u16::MAX)
    }

    pub fn changer_ie_count(&self) -> u16 {
        let count = self.changer_ie_ports.len();
        if count == 0 {
            return 1;
        }
        u16::try_from(count).unwrap_or(u16::MAX)
    }
}

fn initial_unit_attention_queue() -> VecDeque<(u8, u8)> {
    let mut queue = VecDeque::new();
    queue.push_back((0x29, 0x00)); // POWER ON, RESET OR BUS DEVICE RESET OCCURRED
    queue
}

fn normalize_nexus_key(initiator: &str) -> String {
    initiator.trim().to_string()
}

fn default_changer_slots() -> BTreeMap<u16, Option<String>> {
    let mut slots = BTreeMap::new();
    for idx in 0..16u16 {
        let addr = 0x0400u16 + idx;
        if idx < 10 {
            slots.insert(addr, Some(format!("VTL{:06}", idx + 1)));
        } else {
            slots.insert(addr, None);
        }
    }
    slots
}

fn default_changer_drives() -> BTreeMap<u16, Option<String>> {
    let mut drives = BTreeMap::new();
    drives.insert(0x0100, None);
    drives
}

fn default_changer_ie_ports() -> BTreeMap<u16, Option<String>> {
    let mut ie_ports = BTreeMap::new();
    ie_ports.insert(0x0300, None);
    ie_ports
}

fn default_changer_ie_impexp() -> BTreeMap<u16, bool> {
    let mut flags = BTreeMap::new();
    flags.insert(0x0300, false);
    flags
}

fn default_changer_drive_sources() -> BTreeMap<u16, Option<u16>> {
    let mut sources = BTreeMap::new();
    sources.insert(0x0100, None);
    sources
}
