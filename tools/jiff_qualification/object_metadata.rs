use datafusion::arrow::datatypes::Schema;
use datafusion::common::Statistics;
use datafusion::datasource::listing::PartitionedFile;
use datafusion::execution::cache::cache_manager::CachedFileMetadata;
use datafusion::object_store::local::LocalFileSystem;
use datafusion::object_store::path::Path;
use datafusion::object_store::ObjectStoreExt;
use std::fs::{File, FileTimes};
use std::sync::Arc;
use std::time::{Duration, SystemTime};

#[test]
fn partitioned_file_metadata_uses_signed_instants() {
    let file = PartitionedFile::new("quotes.parquet", 32);
    let ranged = PartitionedFile::new_with_range("quotes.parquet".into(), 32, 0, 8);
    assert_eq!(file.object_meta.last_modified.as_nanosecond(), 0);
    assert_eq!(file.object_meta, ranged.object_meta);

    let root = std::env::temp_dir();
    let name = format!("kanz-object-mtime-{}.parquet", std::process::id());
    let fixture_path = root.join(&name);
    let fixture = File::create_new(&fixture_path).unwrap();
    let store = LocalFileSystem::new_with_prefix(root).unwrap();
    let path = Path::from(name);
    let runtime = tokio::runtime::Builder::new_current_thread()
        .build()
        .unwrap();

    let epoch = SystemTime::UNIX_EPOCH;
    let tick = Duration::from_nanos(1);
    for (time, nanos) in [
        (epoch - tick, -1),
        (epoch, 0),
        (epoch + tick, 1),
        (
            epoch + Duration::from_secs(7_258_118_400),
            7_258_118_400_000_000_000,
        ),
    ] {
        fixture
            .set_times(FileTimes::new().set_modified(time))
            .unwrap();
        assert_eq!(fixture.metadata().unwrap().modified().unwrap(), time);
        let observed = runtime.block_on(store.head(&path)).unwrap();
        let restored = PartitionedFile::new_from_meta(observed.clone());
        assert_eq!(restored.object_meta.last_modified.as_nanosecond(), nanos);
        let cached = CachedFileMetadata {
            meta: observed.clone(),
            statistics: Arc::new(Statistics::new_unknown(&Schema::empty())),
            ordering: None,
        };
        assert!(cached.is_valid_for(&observed));
        let mut stale = observed.clone();
        stale.last_modified = stale.last_modified.checked_add(tick).unwrap();
        assert!(!cached.is_valid_for(&stale));
        let mut resized = observed;
        resized.size += 1;
        assert!(!cached.is_valid_for(&resized));
    }
    let too_late = epoch + Duration::from_secs(253_402_300_800);
    fixture
        .set_times(FileTimes::new().set_modified(too_late))
        .unwrap();
    assert_eq!(fixture.metadata().unwrap().modified().unwrap(), too_late);
    let refusal = runtime.block_on(store.head(&path)).unwrap_err();
    assert!(refusal.to_string().contains(&format!("Unable to access metadata for {path}")));
    std::fs::remove_file(fixture_path).unwrap();
}
