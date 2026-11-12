//! Exact gross weighted-average cost, shared by OMS and the accounting candidate.
//! Mirrors `kanz/internal/costbasis`; fees remain an explicit caller-owned leg.
use num_rational::BigRational as Rat;
use num_traits::{Signed, Zero};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    Capacity,
    InvalidRational,
}

pub fn bound(value: &Rat) -> Result<(), Error> {
    if value.numer().bits() > 16384 || value.denom().bits() > 16384 {
        Err(Error::Capacity)
    } else {
        // BigRational::new_raw permits zero/negative denominators and unreduced
        // values. Go big.Rat inputs cannot carry that representation. Refuse it
        // before sign/zero tests or arithmetic can panic or misinterpret it.
        if !value.denom().is_positive() {
            return Err(Error::InvalidRational);
        }
        let reduced = value.reduced();
        if reduced.numer() != value.numer() || reduced.denom() != value.denom() {
            return Err(Error::InvalidRational);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Lot {
    pub qty: Rat,
    pub avg: Rat,
    pub realized: Rat,
}

impl Lot {
    /// A rejected operation leaves the lot unchanged. Inputs and intermediates
    /// are bounded before another operation can amplify their allocation.
    pub fn fold(&mut self, quantity: &Rat, price: &Rat) -> Result<(), Error> {
        for value in [&self.qty, &self.avg, &self.realized, quantity, price] {
            bound(value)?;
        }
        let mut next = self.clone();
        next.qty += quantity;
        if self.qty.is_zero() || self.qty.signum() == quantity.signum() {
            next.avg = if next.qty.is_zero() {
                Rat::zero()
            } else {
                (self.qty.abs() * &self.avg + quantity.abs() * price) / next.qty.abs()
            };
        } else {
            let closed = self.qty.abs().min(quantity.abs());
            next.realized += closed * (price - &self.avg) * self.qty.signum();
            if next.qty.is_zero() {
                next.avg = Rat::zero();
            } else if next.qty.signum() != self.qty.signum() {
                next.avg = price.clone();
            }
        }
        for value in [&next.qty, &next.avg, &next.realized] {
            bound(value)?;
        }
        *self = next;
        Ok(())
    }
}

pub fn aggregate<'a>(lots: impl IntoIterator<Item = &'a Lot>) -> Result<Lot, Error> {
    let mut out = Lot::default();
    let mut weight = Rat::zero();
    let mut cost = Rat::zero();
    for lot in lots {
        for value in [&lot.qty, &lot.avg, &lot.realized] {
            bound(value)?;
        }
        out.qty += &lot.qty;
        out.realized += &lot.realized;
        weight += lot.qty.abs();
        cost += lot.qty.abs() * &lot.avg;
        for value in [&out.qty, &out.realized, &weight, &cost] {
            bound(value)?;
        }
    }
    if !weight.is_zero() {
        out.avg = cost / weight;
    }
    bound(&out.avg)?;
    Ok(out)
}
