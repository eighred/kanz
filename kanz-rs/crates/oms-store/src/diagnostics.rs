//! Qualification-only stage observations, never admission or retry inputs.
//! AckTargets/AckReceived are nested within DurableAck; Total includes all owner
//! and fixture stages. These durations are explicitly NONADDITIVE.
use std::future::Future;

pub const STAGES: usize = 14;
#[derive(Clone, Copy)]
#[repr(usize)]
pub enum Stage {
    HandlerConnect,
    LoadCase,
    LoadOrder,
    DurableAck,
    AckTargets,
    AckReceived,
    HandlerClose,
    RelayConnect,
    RelayAcquire,
    RelayPending,
    RelayRelease,
    RelayClose,
    FixtureAbsence,
    Total,
}
#[derive(Clone, Copy, Default)]
pub struct Snapshot {
    pub counts: [u64; STAGES],
    pub nanoseconds: [u64; STAGES],
}
#[cfg(feature = "qualification-diagnostics")]
tokio::task_local! {
    static CURRENT: std::cell::RefCell<Snapshot>;
}

pub struct Timer {
    #[cfg(feature = "qualification-diagnostics")]
    stage: Stage,
    #[cfg(feature = "qualification-diagnostics")]
    started: std::time::Instant,
}
impl Timer {
    pub fn new(stage: Stage) -> Self {
        #[cfg(not(feature = "qualification-diagnostics"))]
        let _ = stage;
        Self {
            #[cfg(feature = "qualification-diagnostics")]
            stage,
            #[cfg(feature = "qualification-diagnostics")]
            started: std::time::Instant::now(),
        }
    }
}
impl Drop for Timer {
    fn drop(&mut self) {
        #[cfg(feature = "qualification-diagnostics")]
        {
            let elapsed = self.started.elapsed().as_nanos().min(u128::from(u64::MAX)) as u64;
            let _ = CURRENT.try_with(|current| {
                let mut value = current.borrow_mut();
                add(&mut value, self.stage, elapsed);
            });
        }
    }
}
#[cfg(feature = "qualification-diagnostics")]
fn add(value: &mut Snapshot, stage: Stage, elapsed: u64) {
    let index = stage as usize;
    value.counts[index] = value.counts[index].saturating_add(1);
    value.nanoseconds[index] = value.nanoseconds[index].saturating_add(elapsed);
}
pub async fn measure<F: Future>(stage: Stage, work: F) -> F::Output {
    let _timer = Timer::new(stage);
    work.await
}
pub async fn capture<F: Future>(work: F) -> (F::Output, Snapshot) {
    #[cfg(feature = "qualification-diagnostics")]
    {
        CURRENT
            .scope(std::cell::RefCell::new(Snapshot::default()), async {
                let result = work.await;
                (result, CURRENT.with(|value| *value.borrow()))
            })
            .await
    }
    #[cfg(not(feature = "qualification-diagnostics"))]
    {
        (work.await, Snapshot::default())
    }
}

#[cfg(all(test, feature = "qualification-diagnostics"))]
mod tests {
    use super::*;
    #[test]
    fn counters_saturate_without_allocating() {
        let mut s = Snapshot::default();
        s.counts[0] = u64::MAX;
        s.nanoseconds[0] = u64::MAX;
        add(&mut s, Stage::HandlerConnect, 1);
        assert_eq!(s.counts[0], u64::MAX);
        assert_eq!(s.nanoseconds[0], u64::MAX);
        assert_eq!(std::mem::size_of::<Snapshot>(), STAGES * 16);
    }
    #[tokio::test]
    async fn captures_are_isolated_and_cancelled_stage_is_counted() {
        let (_, one) = capture(async {
            let _ = tokio::time::timeout(
                std::time::Duration::from_millis(1),
                measure(Stage::DurableAck, std::future::pending::<()>()),
            )
            .await;
        })
        .await;
        assert_eq!(one.counts[Stage::DurableAck as usize], 1);
        let (_, two) = capture(async {}).await;
        assert_eq!(two.counts, [0; STAGES]);
    }
}
