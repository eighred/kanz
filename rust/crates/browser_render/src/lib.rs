//! Exact presentation of existing Decimal/RatString values, never valuation.
//! No imports, allocation, pointers, float, rounding or institutional authority.
use std::cell::RefCell;

pub const MAX_NUMERAL: usize = 128;
pub const MAX_INPUT: usize = MAX_NUMERAL * 2 + 1;
pub const MAX_OUTPUT: usize = 256;
pub const MAX_EXPONENT: i32 = 64;
pub const OK: u32 = 1;
pub const UNSUPPORTED: u32 = 2;

#[derive(Clone)]
pub struct Rendered {
    bytes: [u8; MAX_OUTPUT],
    len: usize,
}
impl Rendered {
    const fn empty() -> Self {
        Self {
            bytes: [0; MAX_OUTPUT],
            len: 0,
        }
    }
    fn push(&mut self, byte: u8) -> Option<()> {
        *self.bytes.get_mut(self.len)? = byte;
        self.len += 1;
        Some(())
    }
    pub fn as_bytes(&self) -> &[u8] {
        &self.bytes[..self.len]
    }
}

fn numeral(raw: &[u8], signed: bool) -> Option<(bool, &[u8])> {
    if raw.is_empty() || raw.len() > MAX_NUMERAL {
        return None;
    }
    let negative = signed && raw[0] == b'-';
    let digits = if negative { &raw[1..] } else { raw };
    if digits.is_empty() || !digits.iter().all(u8::is_ascii_digit) {
        return None;
    }
    Some((negative, digits))
}
fn trimmed(digits: &[u8]) -> &[u8] {
    let first = digits
        .iter()
        .position(|&d| d != b'0')
        .unwrap_or(digits.len() - 1);
    &digits[first..]
}
fn decimal_body(digits: &[u8], negative: bool, exponent: i32) -> Option<Rendered> {
    let mut out = Rendered::empty();
    if negative && digits.iter().any(|&d| d != b'0') {
        out.push(b'-')?;
    }
    if exponent >= 0 {
        let digits = trimmed(digits);
        for &d in digits {
            out.push(d)?;
        }
        // TS strips all leading zeros after adding positive-exponent zeros.
        if digits != b"0" {
            for _ in 0..exponent {
                out.push(b'0')?;
            }
        }
    } else {
        let scale = (-exponent) as usize;
        if digits.len() <= scale {
            out.push(b'0')?;
            out.push(b'.')?;
            for _ in digits.len()..scale {
                out.push(b'0')?;
            }
            for &d in digits {
                out.push(d)?;
            }
        } else {
            let cut = digits.len() - scale;
            for &d in trimmed(&digits[..cut]) {
                out.push(d)?;
            }
            out.push(b'.')?;
            for &d in &digits[cut..] {
                out.push(d)?;
            }
        }
    }
    Some(out)
}
pub fn decimal(raw: &[u8], exponent: i32) -> Option<Rendered> {
    if !(-MAX_EXPONENT..=MAX_EXPONENT).contains(&exponent) {
        return None;
    }
    let (negative, digits) = numeral(raw, true)?;
    decimal_body(digits, negative, exponent)
}

// Fixed decimal digits implement only exact small-factor operations required by
// the existing renderer. This is not a general financial arithmetic library.
struct Digits {
    bytes: [u8; MAX_OUTPUT],
    len: usize,
}
impl Digits {
    fn from(raw: &[u8]) -> Self {
        let raw = trimmed(raw);
        let mut result = Self {
            bytes: [0; MAX_OUTPUT],
            len: raw.len(),
        };
        result.bytes[..raw.len()].copy_from_slice(raw);
        result
    }
    fn last(&self) -> u8 {
        self.bytes[self.len - 1] - b'0'
    }
    fn divide(&mut self, divisor: u8) {
        let mut remainder = 0;
        for digit in &mut self.bytes[..self.len] {
            let value = remainder * 10 + *digit - b'0';
            *digit = b'0' + value / divisor;
            remainder = value % divisor;
        }
        let first = self.bytes[..self.len]
            .iter()
            .position(|&d| d != b'0')
            .unwrap_or(self.len - 1);
        self.bytes.copy_within(first..self.len, 0);
        self.len -= first;
    }
    fn multiply(&mut self, factor: u8) -> Option<()> {
        let mut carry = 0;
        for digit in self.bytes[..self.len].iter_mut().rev() {
            let value = (*digit - b'0') * factor + carry;
            *digit = b'0' + value % 10;
            carry = value / 10;
        }
        if carry != 0 {
            if self.len == MAX_OUTPUT {
                return None;
            }
            self.bytes.copy_within(0..self.len, 1);
            self.bytes[0] = b'0' + carry;
            self.len += 1;
        }
        Some(())
    }
}
pub fn rational(raw: &[u8]) -> Option<Rendered> {
    if raw.is_empty() || raw.len() > MAX_INPUT {
        return None;
    }
    let (num, den) = match raw.iter().position(|&b| b == b'/') {
        Some(i) => (&raw[..i], &raw[i + 1..]),
        None => (raw, b"1".as_slice()),
    };
    let (negative, num) = numeral(num, true)?;
    let (_, den) = numeral(den, false)?;
    let mut denominator = Digits::from(den);
    if denominator.bytes[..denominator.len] == *b"0" {
        return None;
    }
    let mut twos = 0;
    let mut fives = 0;
    while denominator.last().is_multiple_of(2) {
        denominator.divide(2);
        twos += 1;
        if twos > MAX_EXPONENT {
            return None;
        }
    }
    while denominator.last() == 0 || denominator.last() == 5 {
        denominator.divide(5);
        fives += 1;
        if fives > MAX_EXPONENT {
            return None;
        }
    }
    if denominator.bytes[..denominator.len] != *b"1" {
        return None;
    }
    let scale = twos.max(fives);
    let mut numerator = Digits::from(num);
    for _ in twos..scale {
        numerator.multiply(2)?;
    }
    for _ in fives..scale {
        numerator.multiply(5)?;
    }
    decimal_body(&numerator.bytes[..numerator.len], negative, -scale)
}

struct State {
    input: [u8; MAX_INPUT],
    len: usize,
    poisoned: bool,
    output: Rendered,
    status: u32,
}
impl State {
    const fn new() -> Self {
        Self {
            input: [0; MAX_INPUT],
            len: 0,
            poisoned: false,
            output: Rendered::empty(),
            status: UNSUPPORTED,
        }
    }
}
thread_local! { static STATE: RefCell<State> = const { RefCell::new(State::new()) }; }

// The ABI never accepts an address or a length that can access host memory.
// Invalid append poisons the request until reset; every render clears old output.
#[no_mangle]
pub extern "C" fn render_reset() {
    STATE.with(|s| *s.borrow_mut() = State::new());
}
#[no_mangle]
pub extern "C" fn render_input_byte(index: u32, value: u32) -> u32 {
    STATE.with(|cell| {
        let mut s = cell.borrow_mut();
        s.status = UNSUPPORTED;
        s.output = Rendered::empty();
        if s.poisoned || index as usize != s.len || s.len == MAX_INPUT || value > 255 {
            s.poisoned = true;
            return UNSUPPORTED;
        }
        let index = s.len;
        s.input[index] = value as u8;
        s.len += 1;
        OK
    })
}
#[no_mangle]
pub extern "C" fn render_value(kind: u32, exponent: i32) -> u32 {
    STATE.with(|cell| {
        let mut s = cell.borrow_mut();
        s.output = Rendered::empty();
        s.status = UNSUPPORTED;
        if s.poisoned {
            return UNSUPPORTED;
        }
        let value = match kind {
            1 => decimal(&s.input[..s.len], exponent),
            2 if exponent == 0 => rational(&s.input[..s.len]),
            _ => None,
        };
        if let Some(out) = value {
            s.output = out;
            s.status = OK;
        }
        s.status
    })
}
#[no_mangle]
pub extern "C" fn render_output_len() -> u32 {
    STATE.with(|s| {
        let s = s.borrow();
        if s.status == OK {
            s.output.len as u32
        } else {
            0
        }
    })
}
#[no_mangle]
pub extern "C" fn render_output_byte(index: u32) -> u32 {
    STATE.with(|s| {
        let s = s.borrow();
        if s.status == OK && (index as usize) < s.output.len {
            u32::from(s.output.bytes[index as usize])
        } else {
            256
        }
    })
}
