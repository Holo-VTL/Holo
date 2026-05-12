use super::metadata::{checked_usize_from_u64, StorageError};
use flate2::read::ZlibDecoder;
use flate2::write::ZlibEncoder;
use flate2::Compression;
use std::io::{Read, Write};

pub const MAX_DECOMPRESSED_PAYLOAD_LEN: usize = 16 << 20;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum CompressionCodec {
    None = 0,
    Rle = 1,
    Lz4 = 2,
    Zlib = 3,
}

impl CompressionCodec {
    pub fn from_u8(value: u8) -> Result<Self, StorageError> {
        match value {
            0 => Ok(Self::None),
            1 => Ok(Self::Rle),
            2 => Ok(Self::Lz4),
            3 => Ok(Self::Zlib),
            _ => Err(StorageError::Corrupt(
                "invalid compression codec".to_string(),
            )),
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CompressionStats {
    pub input_bytes: u64,
    pub stored_bytes: u64,
    // Aggregate count across all codecs; codec-specific counters below break it down.
    pub compressed_writes: u64,
    pub bypassed_writes: u64,
    pub rle_writes: u64,
    pub lz4_writes: u64,
    pub zlib_writes: u64,
    pub dedup_hits: u64,
    pub collision_events: u64,
}

impl CompressionStats {
    pub fn record_write(
        &mut self,
        input_len: usize,
        stored_len: usize,
        codec: CompressionCodec,
        compressed: bool,
    ) {
        self.input_bytes += input_len as u64;
        self.stored_bytes += stored_len as u64;
        if compressed && codec != CompressionCodec::None {
            self.compressed_writes += 1;
            match codec {
                CompressionCodec::Rle => self.rle_writes += 1,
                CompressionCodec::Lz4 => self.lz4_writes += 1,
                CompressionCodec::Zlib => self.zlib_writes += 1,
                CompressionCodec::None => {}
            }
        } else {
            self.bypassed_writes += 1;
        }
    }

    pub fn record_dedup_hit(&mut self) {
        self.dedup_hits += 1;
    }

    pub fn record_collision(&mut self) {
        self.collision_events += 1;
    }

    pub fn compression_ratio(&self) -> f64 {
        if self.input_bytes == 0 {
            1.0
        } else {
            self.stored_bytes as f64 / self.input_bytes as f64
        }
    }
}

pub fn compress_payload(
    codec: CompressionCodec,
    payload: &[u8],
) -> Result<(CompressionCodec, Vec<u8>, bool), StorageError> {
    match codec {
        CompressionCodec::None => Ok((CompressionCodec::None, payload.to_vec(), false)),
        CompressionCodec::Rle => {
            let encoded = rle_encode(payload);
            if encoded.len() >= payload.len() {
                Ok((CompressionCodec::None, payload.to_vec(), false))
            } else {
                Ok((CompressionCodec::Rle, encoded, true))
            }
        }
        CompressionCodec::Lz4 => {
            let encoded = lz4_flex::compress_prepend_size(payload);
            if encoded.len() >= payload.len() {
                Ok((CompressionCodec::None, payload.to_vec(), false))
            } else {
                Ok((CompressionCodec::Lz4, encoded, true))
            }
        }
        CompressionCodec::Zlib => {
            let mut encoder = ZlibEncoder::new(Vec::new(), Compression::default());
            encoder.write_all(payload)?;
            let encoded = encoder.finish()?;
            if encoded.len() >= payload.len() {
                Ok((CompressionCodec::None, payload.to_vec(), false))
            } else {
                Ok((CompressionCodec::Zlib, encoded, true))
            }
        }
    }
}

pub fn decompress_payload(
    codec: CompressionCodec,
    payload: &[u8],
    expected_len: usize,
) -> Result<Vec<u8>, StorageError> {
    ensure_decompressed_len_within_limit(expected_len)?;
    match codec {
        CompressionCodec::None => {
            if payload.len() != expected_len {
                return Err(StorageError::Corrupt(
                    "raw payload length mismatch".to_string(),
                ));
            }
            Ok(payload.to_vec())
        }
        CompressionCodec::Rle => {
            let decoded = rle_decode(payload)?;
            if decoded.len() != expected_len {
                return Err(StorageError::Corrupt(
                    "decoded payload length mismatch".to_string(),
                ));
            }
            Ok(decoded)
        }
        CompressionCodec::Lz4 => {
            let declared_len = lz4_prepended_len(payload)?;
            if declared_len != expected_len {
                return Err(StorageError::Corrupt(
                    "decoded payload length mismatch".to_string(),
                ));
            }
            let decoded = lz4_flex::decompress_size_prepended(payload)
                .map_err(|err| StorageError::Corrupt(format!("lz4 decompress: {err}")))?;
            if decoded.len() != expected_len {
                return Err(StorageError::Corrupt(
                    "decoded payload length mismatch".to_string(),
                ));
            }
            Ok(decoded)
        }
        CompressionCodec::Zlib => {
            let mut decoder = ZlibDecoder::new(payload);
            let mut decoded = Vec::with_capacity(expected_len);
            decoder
                .by_ref()
                .take(expected_len.saturating_add(1) as u64)
                .read_to_end(&mut decoded)?;
            if decoded.len() != expected_len {
                return Err(StorageError::Corrupt(
                    "decoded payload length mismatch".to_string(),
                ));
            }
            Ok(decoded)
        }
    }
}

fn ensure_decompressed_len_within_limit(expected_len: usize) -> Result<(), StorageError> {
    if expected_len > MAX_DECOMPRESSED_PAYLOAD_LEN {
        return Err(StorageError::Corrupt(format!(
            "decompressed payload length {expected_len} exceeds maximum {MAX_DECOMPRESSED_PAYLOAD_LEN}"
        )));
    }
    Ok(())
}

fn lz4_prepended_len(payload: &[u8]) -> Result<usize, StorageError> {
    let header = payload
        .get(..4)
        .ok_or_else(|| StorageError::Corrupt("lz4 payload missing size header".to_string()))?;
    let declared = checked_usize_from_u64(
        u64::from(u32::from_le_bytes([
            header[0], header[1], header[2], header[3],
        ])),
        "lz4 decompressed length",
    )?;
    ensure_decompressed_len_within_limit(declared)?;
    Ok(declared)
}

fn rle_encode(payload: &[u8]) -> Vec<u8> {
    if payload.is_empty() {
        return Vec::new();
    }

    let mut out = Vec::with_capacity(payload.len());
    let mut i = 0usize;
    while i < payload.len() {
        let b = payload[i];
        let mut run = 1usize;
        while i + run < payload.len() && payload[i + run] == b && run < u8::MAX as usize {
            run += 1;
        }
        out.push(run as u8);
        out.push(b);
        i += run;
    }

    out
}

fn rle_decode(payload: &[u8]) -> Result<Vec<u8>, StorageError> {
    if !payload.len().is_multiple_of(2) {
        return Err(StorageError::Corrupt("rle payload malformed".to_string()));
    }

    let mut out = Vec::new();
    let mut i = 0usize;
    while i < payload.len() {
        let run = payload[i] as usize;
        let value = payload[i + 1];
        if run == 0 {
            return Err(StorageError::Corrupt(
                "rle run length cannot be zero".to_string(),
            ));
        }
        let current_len = out.len();
        out.resize(current_len + run, value);
        i += 2;
    }

    Ok(out)
}
