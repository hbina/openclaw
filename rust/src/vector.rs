use thiserror::Error;

#[derive(Debug, Error, PartialEq, Eq)]
pub enum VectorError {
    #[error("embedding byte length {actual} does not match dimensions {dimensions}")]
    WrongLength { actual: usize, dimensions: usize },
    #[error("embedding contains a non-finite value")]
    NonFinite,
}

pub fn pack(vector: &[f32]) -> Vec<u8> {
    vector
        .iter()
        .flat_map(|value| value.to_le_bytes())
        .collect()
}

pub fn unpack(data: &[u8], dimensions: usize) -> Result<Vec<f32>, VectorError> {
    if data.len() != dimensions.saturating_mul(4) {
        return Err(VectorError::WrongLength {
            actual: data.len(),
            dimensions,
        });
    }
    data.as_chunks::<4>()
        .0
        .iter()
        .map(|chunk| {
            let value = f32::from_le_bytes(*chunk);
            value
                .is_finite()
                .then_some(value)
                .ok_or(VectorError::NonFinite)
        })
        .collect()
}

pub fn dot(left: &[f32], right: &[f32]) -> f64 {
    left.iter()
        .zip(right)
        .map(|(left, right)| f64::from(*left) * f64::from(*right))
        .sum()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pack_unpack_round_trip() {
        let values = [1.25, -2.5, 0.0];
        assert_eq!(unpack(&pack(&values), 3).unwrap(), values);
    }

    #[test]
    fn unpack_rejects_bad_data() {
        assert!(matches!(
            unpack(&[0; 4], 2),
            Err(VectorError::WrongLength { .. })
        ));
        assert_eq!(
            unpack(&f32::NAN.to_le_bytes(), 1),
            Err(VectorError::NonFinite)
        );
    }

    #[test]
    fn computes_dot_product_and_ignores_unpaired_tail() {
        assert_eq!(dot(&[1.0, 2.0, 50.0], &[3.0, 4.0]), 11.0);
    }
}
