use std::collections::HashMap;
use std::fs::{self, File, OpenOptions};
use std::io::{ErrorKind, IoSlice, Read, Seek, SeekFrom, Write};
use std::os::unix::fs::FileExt;
use std::path::Path;
use std::path::PathBuf;
use std::sync::{Mutex, OnceLock};

use super::layout::{
    integrity32, SegmentHeader, SegmentKind, STORAGE_LAYOUT_MAGIC, STORAGE_LAYOUT_VERSION,
    STORAGE_LAYOUT_VERSION_V1,
};
use super::metadata::{lock_storage_mutex, StorageError};

pub const SEGMENT_HEADER_SIZE: usize = 36;
pub const SEGMENT_HEADER_V2_COPY_SIZE: usize = 48;
pub const SEGMENT_HEADER_V2_TOTAL_SIZE: usize = SEGMENT_HEADER_V2_COPY_SIZE * 2;

#[derive(Debug)]
struct AppendFileState {
    file: File,
    header: SegmentHeader,
    payload_offset: u64,
    checked_prefix: Option<Vec<u8>>,
    append_offset: u64,
}

fn known_parent_dirs() -> &'static Mutex<Vec<PathBuf>> {
    static KNOWN: OnceLock<Mutex<Vec<PathBuf>>> = OnceLock::new();
    KNOWN.get_or_init(|| Mutex::new(Vec::new()))
}

fn append_file_cache() -> &'static Mutex<HashMap<PathBuf, AppendFileState>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, AppendFileState>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

fn invalidate_append_file(path: &Path) -> Result<(), StorageError> {
    lock_storage_mutex(append_file_cache(), "segment append")?.remove(path);
    Ok(())
}

fn ensure_parent_dir(path: &Path) -> Result<(), StorageError> {
    let Some(parent) = path.parent() else {
        return Ok(());
    };
    {
        let guard = lock_storage_mutex(known_parent_dirs(), "segment parent")?;
        if guard.iter().any(|known| known == parent) {
            return Ok(());
        }
    }
    fs::create_dir_all(parent)?;
    let mut guard = lock_storage_mutex(known_parent_dirs(), "segment parent")?;
    if !guard.iter().any(|known| known == parent) {
        guard.push(parent.to_path_buf());
    }
    Ok(())
}

pub fn write_segment_file(
    path: &Path,
    kind: SegmentKind,
    segment_id: u64,
    sequence: u64,
    payload: &[u8],
) -> Result<(), StorageError> {
    ensure_parent_dir(path)?;
    invalidate_append_file(path)?;
    let header = SegmentHeader::new(kind, segment_id, sequence, payload);
    let tmp_path = path.with_extension("tmp");
    let mut file = OpenOptions::new()
        .create(true)
        .write(true)
        .truncate(true)
        .open(&tmp_path)?;

    write_segment_headers(&mut file, &header)?;
    file.write_all(payload)?;
    file.sync_all()?;
    drop(file);
    fs::rename(&tmp_path, path)?;
    if let Some(parent) = path.parent() {
        sync_directory(parent)?;
    }
    invalidate_append_file(path)?;
    Ok(())
}

pub fn read_segment_file(
    path: &Path,
    expected_kind: SegmentKind,
) -> Result<(SegmentHeader, Vec<u8>), StorageError> {
    if !path.exists() {
        return Err(StorageError::NotFound(format!(
            "segment not found: {}",
            path.display()
        )));
    }

    let mut file = File::open(path)?;
    let mut bytes = Vec::new();
    file.read_to_end(&mut bytes)?;
    if bytes.len() < SEGMENT_HEADER_SIZE {
        return Err(StorageError::Corrupt(format!(
            "segment too short: {}",
            path.display()
        )));
    }

    let (header, payload_offset) = decode_segment_header_set(&bytes)?;
    if header.kind != expected_kind {
        return Err(StorageError::Corrupt("segment kind mismatch".to_string()));
    }

    let payload_start = usize::try_from(payload_offset)
        .map_err(|_| StorageError::Corrupt("segment payload offset overflow".to_string()))?;
    if bytes.len() < payload_start {
        return Err(StorageError::Corrupt(
            "segment payload offset exceeds file".to_string(),
        ));
    }
    let payload = bytes[payload_start..].to_vec();
    header.validate(&payload)?;
    Ok((header, payload))
}

pub fn read_segment_header(
    path: &Path,
    expected_kind: SegmentKind,
) -> Result<SegmentHeader, StorageError> {
    if !path.exists() {
        return Err(StorageError::NotFound(format!(
            "segment not found: {}",
            path.display()
        )));
    }

    let mut file = File::open(path)?;
    let mut probe = vec![0u8; SEGMENT_HEADER_V2_TOTAL_SIZE];
    let read_len = file.read(&mut probe)?;
    let (header, _) = decode_segment_header_set(&probe[..read_len])?;
    if header.kind != expected_kind {
        return Err(StorageError::Corrupt("segment kind mismatch".to_string()));
    }
    Ok(header)
}

pub fn segment_payload_offset(header: &SegmentHeader) -> u64 {
    if header.version == STORAGE_LAYOUT_VERSION {
        SEGMENT_HEADER_V2_TOTAL_SIZE as u64
    } else {
        SEGMENT_HEADER_SIZE as u64
    }
}

pub fn validate_segment_shape(
    path: &Path,
    expected_kind: SegmentKind,
) -> Result<SegmentHeader, StorageError> {
    let header = read_segment_header(path, expected_kind)?;
    let actual_len = fs::metadata(path)?.len();
    let expected_len = segment_payload_offset(&header) + header.payload_len;
    if actual_len != expected_len {
        return Err(StorageError::Corrupt(format!(
            "segment size mismatch expected={expected_len} actual={actual_len}: {}",
            path.display()
        )));
    }
    Ok(header)
}

pub fn checksum32_continue(mut acc: u32, data: &[u8]) -> u32 {
    for b in data {
        acc ^= *b as u32;
        acc = acc.wrapping_mul(16777619);
    }
    acc
}

pub fn integrity32_continue(acc: u32, data: &[u8]) -> u32 {
    crc32c::crc32c_append(acc, data)
}

pub(crate) fn encode_v2_header_copy(header: &SegmentHeader) -> Vec<u8> {
    let mut out = Vec::with_capacity(SEGMENT_HEADER_V2_COPY_SIZE);
    out.extend_from_slice(&header.magic.to_le_bytes());
    out.extend_from_slice(&header.version.to_le_bytes());
    out.extend_from_slice(&(header.kind as u16).to_le_bytes());
    out.extend_from_slice(&header.segment_id.to_le_bytes());
    out.extend_from_slice(&header.sequence.to_le_bytes());
    out.extend_from_slice(&header.payload_len.to_le_bytes());
    out.extend_from_slice(&header.checksum.to_le_bytes());
    out.extend_from_slice(&1u16.to_le_bytes());
    out.extend_from_slice(&[0u8; 6]);
    let header_checksum = integrity32(&out);
    out.extend_from_slice(&header_checksum.to_le_bytes());
    out
}

fn decode_v2_header_copy(buf: &[u8]) -> Result<SegmentHeader, StorageError> {
    if buf.len() < SEGMENT_HEADER_V2_COPY_SIZE {
        return Err(StorageError::Corrupt(
            "v2 segment header too short".to_string(),
        ));
    }
    let expected = u32::from_le_bytes(
        buf[44..48]
            .try_into()
            .map_err(|_| StorageError::Corrupt("v2 header checksum parse failed".to_string()))?,
    );
    if integrity32(&buf[..44]) != expected {
        return Err(StorageError::Corrupt(
            "v2 segment header checksum mismatch".to_string(),
        ));
    }
    let header = SegmentHeader::decode(&buf[..SEGMENT_HEADER_SIZE])?;
    if header.version != STORAGE_LAYOUT_VERSION {
        return Err(StorageError::VersionMismatch {
            expected: STORAGE_LAYOUT_VERSION,
            got: header.version,
        });
    }
    Ok(header)
}

fn decode_segment_header_set(buf: &[u8]) -> Result<(SegmentHeader, u64), StorageError> {
    if buf.len() < SEGMENT_HEADER_SIZE {
        return Err(StorageError::Corrupt(
            "segment header too short".to_string(),
        ));
    }

    let legacy = SegmentHeader::decode(&buf[..SEGMENT_HEADER_SIZE]);
    if let Ok(header) = &legacy {
        if header.version == STORAGE_LAYOUT_VERSION_V1 {
            return Ok((header.clone(), SEGMENT_HEADER_SIZE as u64));
        }
    }
    if buf.len() < SEGMENT_HEADER_V2_TOTAL_SIZE {
        return match legacy {
            Ok(header)
                if header.magic == STORAGE_LAYOUT_MAGIC
                    && header.version != STORAGE_LAYOUT_VERSION =>
            {
                Err(StorageError::VersionMismatch {
                    expected: STORAGE_LAYOUT_VERSION,
                    got: header.version,
                })
            }
            Ok(_) => Err(StorageError::Corrupt(
                "v2 segment header set too short".to_string(),
            )),
            Err(err) => Err(err),
        };
    }

    let primary = decode_v2_header_copy(&buf[..SEGMENT_HEADER_V2_COPY_SIZE]).ok();
    let backup =
        decode_v2_header_copy(&buf[SEGMENT_HEADER_V2_COPY_SIZE..SEGMENT_HEADER_V2_TOTAL_SIZE]).ok();
    let selected = match (primary, backup) {
        (Some(left), Some(right)) => {
            if left.sequence >= right.sequence {
                left
            } else {
                right
            }
        }
        (Some(header), None) | (None, Some(header)) => header,
        (None, None) => {
            return Err(StorageError::Corrupt(
                "no valid v2 segment header copy".to_string(),
            ))
        }
    };
    Ok((selected, SEGMENT_HEADER_V2_TOTAL_SIZE as u64))
}

fn write_segment_headers(file: &mut File, header: &SegmentHeader) -> Result<(), StorageError> {
    if header.version == STORAGE_LAYOUT_VERSION {
        let encoded = encode_v2_header_copy(header);
        file.write_all(&encoded)?;
        file.write_all(&encoded)?;
    } else {
        file.write_all(&header.encode())?;
    }
    Ok(())
}

fn write_segment_header_copies_at(file: &File, header: &SegmentHeader) -> Result<(), StorageError> {
    if header.version == STORAGE_LAYOUT_VERSION {
        let encoded = encode_v2_header_copy(header);
        file.write_all_at(&encoded, SEGMENT_HEADER_V2_COPY_SIZE as u64)?;
        file.write_all_at(&encoded, 0)?;
    } else {
        file.write_all_at(&header.encode(), 0)?;
    }
    Ok(())
}

fn write_all_parts(file: &mut File, parts: &[&[u8]]) -> Result<(), StorageError> {
    if parts.len() == 1 {
        file.write_all(parts[0])?;
        return Ok(());
    }

    let mut index = 0usize;
    let mut offset = 0usize;
    while index < parts.len() {
        let slices = parts[index..]
            .iter()
            .enumerate()
            .map(|(part_index, part)| {
                if part_index == 0 {
                    IoSlice::new(&part[offset..])
                } else {
                    IoSlice::new(part)
                }
            })
            .collect::<Vec<_>>();
        let written = file.write_vectored(&slices)?;
        if written == 0 {
            return Err(StorageError::Io(std::io::Error::new(
                ErrorKind::WriteZero,
                "failed to append segment payload",
            )));
        }

        let mut remaining = written;
        while index < parts.len() {
            let available = parts[index].len() - offset;
            if remaining < available {
                offset += remaining;
                break;
            }
            remaining -= available;
            index += 1;
            offset = 0;
            if remaining == 0 {
                break;
            }
        }
    }
    Ok(())
}

pub fn append_segment_payload(
    path: &Path,
    kind: SegmentKind,
    segment_id: u64,
    expected_prefix: &[u8],
    appended: &[u8],
    sync: bool,
) -> Result<SegmentHeader, StorageError> {
    append_segment_payload_parts(
        path,
        kind,
        segment_id,
        expected_prefix,
        &[appended],
        sync,
        true,
    )
}

pub fn append_segment_payload_parts(
    path: &Path,
    kind: SegmentKind,
    segment_id: u64,
    expected_prefix: &[u8],
    appended_parts: &[&[u8]],
    sync: bool,
    checksum_enabled: bool,
) -> Result<SegmentHeader, StorageError> {
    let appended_len = appended_parts.iter().map(|part| part.len()).sum::<usize>();
    if appended_len == 0 {
        return read_segment_header(path, kind);
    }
    ensure_parent_dir(path)?;
    let mut cache = lock_storage_mutex(append_file_cache(), "segment append")?;
    if !cache.contains_key(path) {
        let mut file = match OpenOptions::new().read(true).write(true).open(path) {
            Ok(file) => file,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
                drop(cache);
                let mut payload = Vec::with_capacity(expected_prefix.len() + appended_len);
                payload.extend_from_slice(expected_prefix);
                for part in appended_parts {
                    payload.extend_from_slice(part);
                }
                write_segment_file(path, kind, segment_id, 1, &payload)?;
                return read_segment_header(path, kind);
            }
            Err(err) => return Err(err.into()),
        };
        let mut header_buf = vec![0u8; SEGMENT_HEADER_V2_TOTAL_SIZE];
        let read_len = file.read(&mut header_buf)?;
        let (header, payload_offset) = decode_segment_header_set(&header_buf[..read_len])?;
        let append_offset = payload_offset + header.payload_len;
        cache.insert(
            path.to_path_buf(),
            AppendFileState {
                file,
                header,
                payload_offset,
                checked_prefix: None,
                append_offset,
            },
        );
    }
    let (header_kind, header_payload_len, next_sequence) = {
        let state = cache.get(path).ok_or_else(|| {
            StorageError::NotFound("segment append cache not initialized".to_string())
        })?;
        (
            state.header.kind,
            state.header.payload_len,
            state.header.sequence.saturating_add(1),
        )
    };
    if header_kind != kind {
        return Err(StorageError::Corrupt("segment kind mismatch".to_string()));
    }
    if header_payload_len == 0 {
        drop(cache);
        let mut payload = Vec::with_capacity(expected_prefix.len() + appended_len);
        payload.extend_from_slice(expected_prefix);
        for part in appended_parts {
            payload.extend_from_slice(part);
        }
        write_segment_file(path, kind, segment_id, next_sequence, &payload)?;
        return read_segment_header(path, kind);
    }

    let state = cache.get_mut(path).ok_or_else(|| {
        StorageError::NotFound("segment append cache not initialized".to_string())
    })?;
    let same_prefix = state
        .checked_prefix
        .as_ref()
        .map(|cached| cached.as_slice() == expected_prefix)
        .unwrap_or(false);
    if !same_prefix {
        state.file.seek(SeekFrom::Start(state.payload_offset))?;
        let mut prefix_buf = vec![0u8; expected_prefix.len()];
        state.file.read_exact(&mut prefix_buf)?;
        if prefix_buf != expected_prefix {
            return Err(StorageError::Conflict(format!(
                "segment {} requires migration before append",
                path.display()
            )));
        }
        state.checked_prefix = Some(expected_prefix.to_vec());
    }

    state.file.seek(SeekFrom::Start(state.append_offset))?;
    write_all_parts(&mut state.file, appended_parts)?;
    state.header.sequence = state.header.sequence.saturating_add(1);
    state.header.payload_len = state.header.payload_len.saturating_add(appended_len as u64);
    if checksum_enabled && state.header.checksum != 0 {
        for part in appended_parts {
            state.header.checksum = if state.header.version == STORAGE_LAYOUT_VERSION {
                integrity32_continue(state.header.checksum, part)
            } else {
                checksum32_continue(state.header.checksum, part)
            };
        }
    } else {
        state.header.checksum = 0;
    }
    state.append_offset = state.append_offset.saturating_add(appended_len as u64);
    if sync {
        state.file.sync_data()?;
    }
    write_segment_header_copies_at(&state.file, &state.header)?;
    if sync {
        state.file.sync_all()?;
    } else {
        state.file.flush()?;
    }
    Ok(state.header.clone())
}

pub fn sync_segment_file(path: &Path) -> Result<(), StorageError> {
    let mut cache = lock_storage_mutex(append_file_cache(), "segment append")?;
    if let Some(state) = cache.get_mut(path) {
        state.file.sync_all()?;
        return Ok(());
    }
    let file = OpenOptions::new().read(true).write(true).open(path)?;
    file.sync_all()?;
    Ok(())
}

fn sync_directory(path: &Path) -> Result<(), StorageError> {
    let dir = OpenOptions::new().read(true).open(path)?;
    dir.sync_all()?;
    Ok(())
}
