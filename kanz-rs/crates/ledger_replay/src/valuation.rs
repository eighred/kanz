//! Pure valuation of the traded book, matching accounting.ComputeNAVInCurrency
//! with FXTable snapshots. Go owns authorization, PIT input selection and serving.
use super::{Book, MAX_ENTRIES, MAX_KEY_BYTES};
use kanz_order_core::cost_basis::bound;
use num_rational::BigRational as Rat;
use num_traits::{One, Signed, Zero};
use std::collections::{BTreeMap, BTreeSet};

pub type Quotes = BTreeMap<String, Option<Rat>>;

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Error {
    Numeric(super::Error),
    MissingConverter,
    MissingPrice(String),
    MissingFx(Vec<String>),
}
impl From<kanz_order_core::cost_basis::Error> for Error {
    fn from(value: kanz_order_core::cost_basis::Error) -> Self {
        Self::Numeric(value.into())
    }
}
impl From<super::Error> for Error {
    fn from(value: super::Error) -> Self {
        Self::Numeric(value)
    }
}

/// Numeric result only. Portfolio, valuation time, input provenance and source
/// completeness remain with the caller; this is not a replacement NAV wire type.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Valuation {
    pub total: Rat,
    pub cash: Rat,
    pub security_value: Rat,
    pub accrued: Rat,
    pub local_exposure: BTreeMap<String, Rat>,
}

struct Converter<'a> {
    reporting: &'a str,
    rates: &'a Quotes,
    missing: BTreeSet<String>,
    local: BTreeMap<String, Rat>,
}
impl Converter<'_> {
    fn contribute(&mut self, total: &mut Rat, currency: &str, amount: &Rat) -> Result<(), Error> {
        bound(amount)?;
        super::add(&mut self.local, currency, amount)?;
        // Preserve FXTable's legacy identity for reporting AND blank currency.
        let rate = if currency.is_empty() || currency == self.reporting {
            Rat::one()
        } else {
            let Some(rate) = self.rates.get(currency).and_then(Option::as_ref) else {
                self.missing.insert(currency.to_owned());
                return Ok(());
            };
            bound(rate)?;
            if !rate.is_positive() {
                self.missing.insert(currency.to_owned());
                return Ok(());
            }
            rate.clone()
        };
        let converted = amount * rate;
        bound(&converted)?;
        *total += converted;
        bound(total)?;
        Ok(())
    }
}

/// Exact signed market value + cash + accrued, in units of reporting currency.
/// FX quotes are reporting units per local unit. None is an absent converter;
/// Some(empty) is a configured identity-only table. Missing/blank instrument
/// currencies default to reporting exactly as the Go InstrumentCurrency join.
///
/// Every nonzero bucket must be convertible, even if net local exposure cancels
/// or a nonzero holding has a zero mark. Zero holdings need no price or currency.
/// Errors return no partial valuation. Multiple missing prices select the first
/// instrument in deterministic order; Go's map order does not define that choice.
pub fn value(
    book: &Book,
    reporting: &str,
    prices: &Quotes,
    instrument_currency: &BTreeMap<String, String>,
    rates: Option<&Quotes>,
) -> Result<Valuation, Error> {
    let rates = rates.ok_or(Error::MissingConverter)?;
    let sizes = [
        book.positions.len(),
        book.cash.len(),
        book.accrued.len(),
        prices.len(),
        instrument_currency.len(),
        rates.len(),
    ];
    if sizes
        .into_iter()
        .try_fold(0usize, usize::checked_add)
        .is_none_or(|n| n > MAX_ENTRIES)
        || reporting.len() > MAX_KEY_BYTES
        || book
            .positions
            .keys()
            .chain(book.cash.keys())
            .chain(book.accrued.keys())
            .chain(prices.keys())
            .chain(instrument_currency.keys())
            .chain(instrument_currency.values())
            .chain(rates.keys())
            .any(|s| s.len() > MAX_KEY_BYTES)
    {
        return Err(super::Error::Capacity.into());
    }
    let mut converter = Converter {
        reporting,
        rates,
        missing: BTreeSet::new(),
        local: BTreeMap::new(),
    };
    let mut result = Valuation::default();
    for (instrument, lot) in &book.positions {
        bound(&lot.qty)?;
        if lot.qty.is_zero() {
            continue;
        }
        let price = prices
            .get(instrument)
            .and_then(Option::as_ref)
            .ok_or_else(|| Error::MissingPrice(instrument.clone()))?;
        bound(price)?;
        let currency = instrument_currency
            .get(instrument)
            .filter(|s| !s.is_empty())
            .map_or(reporting, String::as_str);
        converter.contribute(&mut result.security_value, currency, &(&lot.qty * price))?;
    }
    for (currency, amount) in &book.cash {
        bound(amount)?;
        if !amount.is_zero() {
            converter.contribute(&mut result.cash, currency, amount)?;
        }
    }
    for (currency, amount) in &book.accrued {
        bound(amount)?;
        if !amount.is_zero() {
            converter.contribute(&mut result.accrued, currency, amount)?;
        }
    }
    if !converter.missing.is_empty() {
        return Err(Error::MissingFx(converter.missing.into_iter().collect()));
    }
    result.total = &result.cash + &result.security_value;
    bound(&result.total)?;
    result.total += &result.accrued;
    bound(&result.total)?;
    result.local_exposure = converter.local;
    Ok(result)
}
