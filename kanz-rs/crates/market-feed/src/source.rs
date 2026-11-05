use crate::proto::market::v1::{OrderBookDelta, OrderBookSnapshot};
use std::sync::{
    atomic::{AtomicU64, Ordering},
    Arc,
};
/// Independent of the bounded update queue. Zero revokes every queued book.
pub type Validity = Arc<AtomicU64>;
pub fn valid_epoch(validity: &AtomicU64, epoch: u64) -> bool {
    epoch != 0 && validity.load(Ordering::SeqCst) == epoch
}
pub struct InvalidateOnDrop(pub Validity);
impl Drop for InvalidateOnDrop {
    fn drop(&mut self) {
        self.0.store(0, Ordering::SeqCst);
    }
}
#[derive(Debug)]
pub enum Event {
    Reanchoring,
    Snapshot(u64, OrderBookSnapshot),
    Delta(u64, OrderBookDelta),
}
