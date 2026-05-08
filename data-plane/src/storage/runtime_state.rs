use std::fs::{self, OpenOptions};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};

use super::layout::checksum32;
use super::metadata::StorageError;

const FILEMARKS_STATE_FILE: &str = "filemarks.state";
const RETENTION_STATE_FILE: &str = "retention.state";
const MAX_FILEMARKS: usize = 1_000_000;
const MAX_FILEMARKS_STATE_BYTES: u64 = 8 + (MAX_FILEMARKS as u64 * 8);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RetentionRuntimeState {
    pub is_worm_media: bool,
    pub retention_locked: bool,
}

pub fn persist_filemarks(root: &Path, filemarks: &[u64]) -> Result<(), StorageError> {
    let mut payload = Vec::with_capacity(8 + (filemarks.len() * 8));
    payload.extend_from_slice(&(filemarks.len() as u64).to_le_bytes());
    for mark in filemarks {
        payload.extend_from_slice(&mark.to_le_bytes());
    }
    atomic_write(&root.join(FILEMARKS_STATE_FILE), &payload)
}

pub fn load_filemarks(root: &Path) -> Result<Vec<u64>, StorageError> {
    let path = root.join(FILEMARKS_STATE_FILE);
    if !path.exists() {
        return Ok(Vec::new());
    }

    let metadata = fs::metadata(&path)?;
    if metadata.len() > MAX_FILEMARKS_STATE_BYTES {
        return Err(StorageError::Corrupt(
            "filemarks state exceeds maximum supported size".to_string(),
        ));
    }

    let mut bytes = Vec::new();
    OpenOptions::new()
        .read(true)
        .open(&path)?
        .read_to_end(&mut bytes)?;
    if bytes.len() < 8 {
        return Err(StorageError::Corrupt(
            "filemarks state too short".to_string(),
        ));
    }
    let count = u64::from_le_bytes(
        bytes[0..8]
            .try_into()
            .map_err(|_| StorageError::Corrupt("filemarks count parse failed".to_string()))?,
    ) as usize;
    if count > MAX_FILEMARKS {
        return Err(StorageError::Corrupt(
            "filemarks count exceeds maximum supported entries".to_string(),
        ));
    }
    let expected = 8usize.saturating_add(count.saturating_mul(8));
    if bytes.len() != expected {
        return Err(StorageError::Corrupt(
            "filemarks state length mismatch".to_string(),
        ));
    }

    let mut filemarks = Vec::with_capacity(count);
    let mut offset = 8usize;
    for _ in 0..count {
        let mark = u64::from_le_bytes(
            bytes[offset..offset + 8]
                .try_into()
                .map_err(|_| StorageError::Corrupt("filemarks parse failed".to_string()))?,
        );
        filemarks.push(mark);
        offset += 8;
    }
    Ok(filemarks)
}

pub fn persist_retention_state(
    root: &Path,
    is_worm_media: bool,
    retention_locked: bool,
) -> Result<(), StorageError> {
    let mut payload = vec![u8::from(is_worm_media), u8::from(retention_locked), 0, 0];
    let checksum = checksum32(&payload[0..2]);
    payload[2..4].copy_from_slice(&(checksum as u16).to_le_bytes());
    atomic_write(&root.join(RETENTION_STATE_FILE), &payload)
}

pub fn load_retention_state(root: &Path) -> Result<Option<RetentionRuntimeState>, StorageError> {
    let path = root.join(RETENTION_STATE_FILE);
    if !path.exists() {
        return Ok(None);
    }

    let mut payload = [0u8; 4];
    OpenOptions::new()
        .read(true)
        .open(&path)?
        .read_exact(&mut payload)?;
    let found = u16::from_le_bytes([payload[2], payload[3]]) as u32;
    let expected = checksum32(&payload[0..2]) & 0xFFFF;
    if found != expected {
        return Err(StorageError::Corrupt(
            "retention state checksum mismatch".to_string(),
        ));
    }

    Ok(Some(RetentionRuntimeState {
        is_worm_media: payload[0] != 0,
        retention_locked: payload[1] != 0,
    }))
}

fn atomic_write(path: &Path, payload: &[u8]) -> Result<(), StorageError> {
    let parent = path
        .parent()
        .ok_or_else(|| StorageError::NotFound("state path parent missing".to_string()))?;
    fs::create_dir_all(parent)?;
    let tmp_path = tmp_path_for(path);

    let mut file = OpenOptions::new()
        .create(true)
        .write(true)
        .truncate(true)
        .open(&tmp_path)?;
    file.write_all(payload)?;
    file.sync_all()?;
    drop(file);

    fs::rename(&tmp_path, path)?;
    let dir = OpenOptions::new().read(true).open(parent)?;
    dir.sync_all()?;
    Ok(())
}

fn tmp_path_for(path: &Path) -> PathBuf {
    let mut tmp = path.as_os_str().to_os_string();
    tmp.push(".tmp");
    PathBuf::from(tmp)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn temp_root(case: &str) -> PathBuf {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let root = std::env::temp_dir().join(format!("holo-runtime-state-{case}-{nanos}"));
        fs::create_dir_all(&root).expect("create temp root");
        root
    }

    #[test]
    fn load_filemarks_rejects_unbounded_count() {
        let root = temp_root("too-many");
        let path = root.join(FILEMARKS_STATE_FILE);
        let count = (MAX_FILEMARKS as u64).saturating_add(1);
        fs::write(&path, count.to_le_bytes()).expect("write filemarks count");

        let err = load_filemarks(&root).expect_err("count should be rejected");
        assert!(matches!(err, StorageError::Corrupt(_)));
    }

    #[test]
    fn load_filemarks_rejects_oversized_state_file_before_allocation() {
        let root = temp_root("oversized");
        let path = root.join(FILEMARKS_STATE_FILE);
        let file = OpenOptions::new()
            .create(true)
            .write(true)
            .open(&path)
            .expect("create oversized file");
        file.set_len(MAX_FILEMARKS_STATE_BYTES + 1)
            .expect("set oversized length");

        let err = load_filemarks(&root).expect_err("oversized file should be rejected");
        assert!(matches!(err, StorageError::Corrupt(_)));
    }
}
