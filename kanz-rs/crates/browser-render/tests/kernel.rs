use kanz_browser_render::*;

fn text(value: Option<Rendered>) -> Option<String> {
    value.map(|v| String::from_utf8(v.as_bytes().to_vec()).unwrap())
}
#[test]
fn exact_golden_strings() {
    for (raw, exponent, expected) in [
        ("9007199254740993", 0, "9007199254740993"),
        ("-9223372036854775808", -2, "-92233720368547758.08"),
        ("100", -2, "1.00"),
        ("-000", -2, "0.00"),
        ("00012", -4, "0.0012"),
        ("-0", 64, "0"),
        ("5", -3, "0.005"),
        ("12345", 2, "1234500"),
    ] {
        assert_eq!(
            text(decimal(raw.as_bytes(), exponent)).as_deref(),
            Some(expected)
        );
    }
    for (raw, expected) in [
        ("261/2", "130.5"),
        ("1/8", "0.125"),
        ("-0/8", "0.000"),
        ("00012/0005", "2.4"),
        ("6/4", "1.50"),
        ("9007199254740993/100", "90071992547409.93"),
    ] {
        assert_eq!(text(rational(raw.as_bytes())).as_deref(), Some(expected));
    }
}
#[test]
fn exact_domain_and_resource_refusals() {
    for raw in ["", "-", "+1", "1.0", " 1", "1e3", "１"] {
        assert!(decimal(raw.as_bytes(), 0).is_none());
    }
    for exponent in [i32::MIN, -65, 65, i32::MAX] {
        assert!(decimal(b"1", exponent).is_none());
    }
    assert!(decimal(&[b'9'; 128], -64).is_some());
    assert!(decimal(&[b'9'; 129], 0).is_none());
    assert!(decimal(b"1", -64).is_some());
    for raw in ["3/6", "0/3", "1/0", "1/-2", "1/2/3", "1/", "1.5", "/2"] {
        assert!(rational(raw.as_bytes()).is_none());
    }
    assert!(rational(b"1/18446744073709551616").is_some());
    assert!(rational(b"1/36893488147419103232").is_none());
    let mut full = vec![b'0'; MAX_INPUT];
    full[127] = b'1';
    full[128] = b'/';
    full[256] = b'1';
    assert_eq!(text(rational(&full)).as_deref(), Some("1"));
    full.push(b'1');
    assert!(rational(&full).is_none());
}
#[test]
fn abi_is_closed_and_cannot_reuse_stale_output() {
    render_reset();
    assert_eq!(render_output_byte(0), 256);
    assert_eq!(render_input_byte(0, u32::from(b'1')), OK);
    assert_eq!(render_value(1, -2), OK);
    assert_eq!(render_output_len(), 4);
    assert_eq!(render_output_byte(3), u32::from(b'1'));
    assert_eq!(render_output_byte(u32::MAX), 256);
    assert_eq!(render_value(99, 0), UNSUPPORTED);
    assert_eq!(render_output_len(), 0);
    assert_eq!(render_output_byte(0), 256);
    assert_eq!(render_value(1, 0), OK);
    assert_eq!(render_input_byte(2, 50), UNSUPPORTED);
    assert_eq!(render_output_len(), 0);
    assert_eq!(render_input_byte(1, 50), UNSUPPORTED);
    assert_eq!(render_value(1, 0), UNSUPPORTED);
    render_reset();
    assert_eq!(render_input_byte(0, 256), UNSUPPORTED);
    render_reset();
    assert_eq!(render_value(2, 0), UNSUPPORTED);
    for i in 0..MAX_INPUT {
        assert_eq!(render_input_byte(i as u32, 49), OK);
    }
    assert_eq!(render_input_byte(MAX_INPUT as u32, 49), UNSUPPORTED);
    assert_eq!(render_value(1, 0), UNSUPPORTED);
}
