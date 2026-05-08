/// holo TCMU handler binary.
///
/// Entry point for the `user:holo` tcmu-runner plugin. The binary:
///   1. Parses --socket-path and --publication-id from argv.
///   2. Binds a UNIX domain socket at `socket_path`.
///   3. Accepts one connection per LUN and serves CDB frames in a loop,
///      dispatching each CDB to the data-plane tape state machine.
///   4. Handles SIGTERM gracefully: finishes the current CDB, closes the socket.
///
/// Wire protocol: see `data-plane/src/iscsi/cdb_server.rs`
use data_plane::iscsi::cdb_server::{
    dispatch_raw_cdb_with_context, CdbDispatchContext, CdbPacket, CdbResponse, MAX_DATA_LEN,
};
use data_plane::scsi_tape::state::TapeState;
use signal_hook::consts::signal::SIGTERM;
use signal_hook::flag;
use socket2::SockRef;
use std::env;
use std::io::{BufReader, BufWriter, Read, Write};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::UnixListener;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

const DEFAULT_TCMU_IO_TIMEOUT_MS: u64 = 300_000;

fn main() {
    let args: Vec<String> = env::args().collect();

    let socket_path = parse_arg(&args, "--socket-path")
        .unwrap_or_else(|| "/run/holo/cdb-default.sock".to_string());
    let publication_id =
        parse_arg(&args, "--publication-id").unwrap_or_else(|| "default".to_string());
    let serial_seed = resolve_serial_seed(&publication_id);

    eprintln!(
        "[tcmu_handler] starting: publication_id={publication_id} serial_seed={serial_seed} socket={socket_path}"
    );

    // Remove stale socket if it exists.
    let path = PathBuf::from(&socket_path);
    if let Err(err) = std::fs::remove_file(&path) {
        if err.kind() != std::io::ErrorKind::NotFound {
            eprintln!("[tcmu_handler] failed to remove stale socket: {err}");
            std::process::exit(1);
        }
    }

    let listener = UnixListener::bind(&socket_path).unwrap_or_else(|e| {
        eprintln!("[tcmu_handler] bind failed: {e}");
        std::process::exit(1);
    });

    // Restrict the socket to the handler's owner. Without this, the socket
    // inherits umask-derived permissions and may be world-writable, exposing
    // CDB dispatch to any local user. tcmu-runner connects as root or as the
    // dedicated holo service user, both of which satisfy 0o600.
    if let Err(err) = std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)) {
        eprintln!("[tcmu_handler] failed to chmod socket {socket_path}: {err}");
        let _ = std::fs::remove_file(&path);
        std::process::exit(1);
    }

    eprintln!("[tcmu_handler] listening on {socket_path}");

    // Shutdown flag — set by SIGTERM handler.
    let shutdown = Arc::new(AtomicBool::new(false));
    if let Err(err) = flag::register(SIGTERM, Arc::clone(&shutdown)) {
        eprintln!("[tcmu_handler] failed to register SIGTERM handler: {err}");
        std::process::exit(1);
    }

    // Keep runtime tape state for the entire lifetime of this handler process.
    // The TCMU bridge may reconnect after transient socket failures/timeouts;
    // recreating TapeState on every reconnect loses in-memory session state and
    // can surface initiator-visible tape errors mid-job.
    let mut tape_state = TapeState::new(&serial_seed);
    let mut data_buffer = Vec::new();
    let mut timing_probe = TimingProbe::from_env(&socket_path);
    let io_timeout = tcmu_io_timeout();
    let max_frame_bytes = tcmu_max_frame_bytes();

    // Accept one connection per LUN (TCMU model: one handler per device).
    for stream in listener.incoming() {
        if shutdown.load(Ordering::Acquire) {
            break;
        }
        match stream {
            Ok(stream) => {
                if let Err(err) = configure_stream_buffers(&stream) {
                    eprintln!("[tcmu_handler] socket buffer setup error: {err}");
                }
                if let Err(err) = configure_stream_timeouts(&stream, io_timeout) {
                    eprintln!("[tcmu_handler] socket timeout setup error: {err}");
                    break;
                }
                let mut reader = BufReader::new(&stream);
                let mut writer = BufWriter::new(&stream);

                loop {
                    if shutdown.load(Ordering::Acquire) {
                        break;
                    }
                    match CdbPacket::decode_header(&mut reader) {
                        Ok(header) => {
                            if header.data_len > max_frame_bytes {
                                eprintln!(
                                    "[tcmu_handler] frame data length {} exceeds configured maximum {}",
                                    header.data_len, max_frame_bytes
                                );
                                let busy = CdbResponse::busy();
                                let _ = busy.encode(&mut writer);
                                let _ = writer.flush();
                                break;
                            }
                            let opcode = header.cdb.first().copied().unwrap_or(0);
                            let timing_start = timing_probe.mark();
                            if let Err(e) =
                                read_frame_data(&mut reader, header.data_len, &mut data_buffer)
                            {
                                if e.kind() == std::io::ErrorKind::UnexpectedEof {
                                    eprintln!("[tcmu_handler] client disconnected");
                                } else {
                                    eprintln!("[tcmu_handler] read data error: {e}");
                                    let busy = CdbResponse::busy();
                                    let _ = busy.encode(&mut writer);
                                    let _ = writer.flush();
                                }
                                break;
                            }
                            let timing_after_read = timing_probe.mark();
                            let context = CdbDispatchContext {
                                initiator: header.initiator.clone(),
                            };
                            let response = dispatch_raw_cdb_with_context(
                                &mut tape_state,
                                &header.cdb,
                                &data_buffer,
                                context,
                            );
                            let timing_after_dispatch = timing_probe.mark();
                            let reply_len = response.reply.len();
                            let sense_len = response.sense.len();
                            if let Err(e) = response.encode(&mut writer) {
                                eprintln!("[tcmu_handler] write error: {e}");
                                break;
                            }
                            if let Err(e) = writer.flush() {
                                eprintln!("[tcmu_handler] flush error: {e}");
                                break;
                            }
                            let timing_after_flush = timing_probe.mark();
                            timing_probe.record(
                                opcode,
                                header.data_len,
                                reply_len,
                                sense_len,
                                timing_start,
                                timing_after_read,
                                timing_after_dispatch,
                                timing_after_flush,
                            );
                        }
                        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => {
                            eprintln!("[tcmu_handler] client disconnected");
                            break;
                        }
                        Err(e) => {
                            eprintln!("[tcmu_handler] read error: {e}");
                            // Return BUSY for unknown read errors.
                            let busy = CdbResponse::busy();
                            let _ = busy.encode(&mut writer);
                            break;
                        }
                    }
                }
            }
            Err(e) => {
                eprintln!("[tcmu_handler] accept error: {e}");
            }
        }
    }

    // Cleanup.
    let _ = std::fs::remove_file(&socket_path);
    eprintln!("[tcmu_handler] exiting cleanly");
}

fn parse_arg(args: &[String], flag: &str) -> Option<String> {
    args.windows(2).find(|w| w[0] == flag).map(|w| w[1].clone())
}

fn resolve_serial_seed(publication_id: &str) -> String {
    let seed = env::var("HOLO_SCSI_SERIAL_SEED").unwrap_or_default();
    let trimmed = seed.trim();
    if trimmed.is_empty() {
        publication_id.to_string()
    } else {
        trimmed.to_string()
    }
}

fn read_frame_data<R: Read>(
    reader: &mut R,
    data_len: usize,
    buffer: &mut Vec<u8>,
) -> std::io::Result<()> {
    buffer.resize(data_len, 0);
    if data_len == 0 {
        return Ok(());
    }
    reader.read_exact(buffer)
}

fn configure_stream_buffers(stream: &std::os::unix::net::UnixStream) -> std::io::Result<()> {
    let Some(bytes) = socket_buffer_bytes() else {
        return Ok(());
    };
    let sock = SockRef::from(stream);
    sock.set_send_buffer_size(bytes)?;
    sock.set_recv_buffer_size(bytes)?;
    Ok(())
}

fn configure_stream_timeouts(
    stream: &std::os::unix::net::UnixStream,
    timeout: Duration,
) -> std::io::Result<()> {
    stream.set_read_timeout(Some(timeout))?;
    stream.set_write_timeout(Some(timeout))
}

fn socket_buffer_bytes() -> Option<usize> {
    let raw = env::var("HOLO_TCMU_SOCKET_BUF_BYTES").ok()?;
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return None;
    }
    let bytes = trimmed.parse::<usize>().ok()?;
    if !(65_536..=67_108_864).contains(&bytes) {
        return None;
    }
    Some(bytes)
}

fn tcmu_io_timeout() -> Duration {
    let millis = env::var("HOLO_TCMU_IO_TIMEOUT_MS")
        .ok()
        .and_then(|raw| raw.trim().parse::<u64>().ok())
        .filter(|value| *value >= 1_000)
        .unwrap_or(DEFAULT_TCMU_IO_TIMEOUT_MS);
    Duration::from_millis(millis)
}

fn tcmu_max_frame_bytes() -> usize {
    env::var("HOLO_TCMU_MAX_FRAME_BYTES")
        .ok()
        .and_then(|raw| raw.trim().parse::<usize>().ok())
        .filter(|value| (1..=MAX_DATA_LEN).contains(value))
        .unwrap_or(MAX_DATA_LEN)
}

#[derive(Clone, Copy)]
enum TimingBucket {
    Read,
    Write,
    Other,
}

#[derive(Clone, Copy, Default)]
struct TimingStats {
    commands: u64,
    data_out_bytes: u64,
    data_in_bytes: u64,
    sense_bytes: u64,
    read_frame_us: u64,
    dispatch_us: u64,
    encode_flush_us: u64,
    total_us: u64,
    max_total_us: u64,
}

struct TimingProbe {
    socket_path: String,
    every: u64,
    since_log: u64,
    read: TimingStats,
    write: TimingStats,
    other: TimingStats,
}

impl TimingProbe {
    fn from_env(socket_path: &str) -> Self {
        let every = env::var("HOLO_CDB_TIMING_EVERY")
            .ok()
            .and_then(|raw| raw.trim().parse::<u64>().ok())
            .filter(|value| (1..=1_000_000).contains(value))
            .unwrap_or(0);
        Self {
            socket_path: socket_path.to_string(),
            every,
            since_log: 0,
            read: TimingStats::default(),
            write: TimingStats::default(),
            other: TimingStats::default(),
        }
    }

    fn mark(&self) -> Option<Instant> {
        if self.every == 0 {
            None
        } else {
            Some(Instant::now())
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn record(
        &mut self,
        opcode: u8,
        data_out_len: usize,
        data_in_len: usize,
        sense_len: usize,
        start: Option<Instant>,
        after_read: Option<Instant>,
        after_dispatch: Option<Instant>,
        after_flush: Option<Instant>,
    ) {
        if self.every == 0 {
            return;
        }
        let (Some(start), Some(after_read), Some(after_dispatch), Some(after_flush)) =
            (start, after_read, after_dispatch, after_flush)
        else {
            return;
        };

        let read_frame = duration_micros(after_read.duration_since(start));
        let dispatch = duration_micros(after_dispatch.duration_since(after_read));
        let encode_flush = duration_micros(after_flush.duration_since(after_dispatch));
        let total = duration_micros(after_flush.duration_since(start));

        let stats = match timing_bucket_for_opcode(opcode) {
            TimingBucket::Read => &mut self.read,
            TimingBucket::Write => &mut self.write,
            TimingBucket::Other => &mut self.other,
        };
        stats.commands = stats.commands.saturating_add(1);
        stats.data_out_bytes = stats.data_out_bytes.saturating_add(data_out_len as u64);
        stats.data_in_bytes = stats.data_in_bytes.saturating_add(data_in_len as u64);
        stats.sense_bytes = stats.sense_bytes.saturating_add(sense_len as u64);
        stats.read_frame_us = stats.read_frame_us.saturating_add(read_frame);
        stats.dispatch_us = stats.dispatch_us.saturating_add(dispatch);
        stats.encode_flush_us = stats.encode_flush_us.saturating_add(encode_flush);
        stats.total_us = stats.total_us.saturating_add(total);
        stats.max_total_us = stats.max_total_us.max(total);

        self.since_log = self.since_log.saturating_add(1);
        if self.since_log >= self.every {
            self.flush();
        }
    }

    fn flush(&mut self) {
        self.log_bucket("read", self.read);
        self.log_bucket("write", self.write);
        self.log_bucket("other", self.other);
        self.read = TimingStats::default();
        self.write = TimingStats::default();
        self.other = TimingStats::default();
        self.since_log = 0;
    }

    fn log_bucket(&self, bucket: &str, stats: TimingStats) {
        if stats.commands == 0 {
            return;
        }
        eprintln!(
            "[tcmu_handler][timing] socket={} bucket={} cmds={} data_out={} data_in={} sense={} avg_total_us={} avg_read_frame_us={} avg_dispatch_us={} avg_encode_flush_us={} max_total_us={}",
            self.socket_path,
            bucket,
            stats.commands,
            stats.data_out_bytes,
            stats.data_in_bytes,
            stats.sense_bytes,
            avg_us(stats.total_us, stats.commands),
            avg_us(stats.read_frame_us, stats.commands),
            avg_us(stats.dispatch_us, stats.commands),
            avg_us(stats.encode_flush_us, stats.commands),
            stats.max_total_us,
        );
    }
}

impl Drop for TimingProbe {
    fn drop(&mut self) {
        if self.every != 0 {
            self.flush();
        }
    }
}

fn timing_bucket_for_opcode(opcode: u8) -> TimingBucket {
    match opcode {
        0x08 | 0x88 => TimingBucket::Read,
        0x0A | 0x8A => TimingBucket::Write,
        _ => TimingBucket::Other,
    }
}

fn duration_micros(duration: Duration) -> u64 {
    duration.as_micros().min(u128::from(u64::MAX)) as u64
}

fn avg_us(total: u64, count: u64) -> u64 {
    if count == 0 {
        0
    } else {
        total / count
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    static ENV_LOCK: Mutex<()> = Mutex::new(());

    #[test]
    fn resolve_serial_seed_falls_back_to_publication_id() {
        let _guard = ENV_LOCK.lock().expect("lock");
        std::env::remove_var("HOLO_SCSI_SERIAL_SEED");
        assert_eq!(resolve_serial_seed("pub-123"), "pub-123");
    }

    #[test]
    fn resolve_serial_seed_prefers_env_when_set() {
        let _guard = ENV_LOCK.lock().expect("lock");
        std::env::set_var("HOLO_SCSI_SERIAL_SEED", "drive-01");
        assert_eq!(resolve_serial_seed("pub-123"), "drive-01");
        std::env::remove_var("HOLO_SCSI_SERIAL_SEED");
    }

    #[test]
    fn tcmu_io_timeout_uses_default_for_invalid_values() {
        let _guard = ENV_LOCK.lock().expect("lock");
        std::env::set_var("HOLO_TCMU_IO_TIMEOUT_MS", "250");
        assert_eq!(
            tcmu_io_timeout(),
            Duration::from_millis(DEFAULT_TCMU_IO_TIMEOUT_MS)
        );
        std::env::remove_var("HOLO_TCMU_IO_TIMEOUT_MS");
    }

    #[test]
    fn tcmu_max_frame_bytes_respects_configured_budget() {
        let _guard = ENV_LOCK.lock().expect("lock");
        std::env::set_var("HOLO_TCMU_MAX_FRAME_BYTES", "1048576");
        assert_eq!(tcmu_max_frame_bytes(), 1_048_576);
        std::env::set_var("HOLO_TCMU_MAX_FRAME_BYTES", (MAX_DATA_LEN + 1).to_string());
        assert_eq!(tcmu_max_frame_bytes(), MAX_DATA_LEN);
        std::env::remove_var("HOLO_TCMU_MAX_FRAME_BYTES");
    }
}
