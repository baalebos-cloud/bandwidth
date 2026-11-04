"""
Trains the session fraud-scoring model.

This is real scikit-learn training code, not a stub — it generates labeled
synthetic data whose structure mirrors the fraud patterns actually
described in the platform blueprint (byte-count divergence, low node trust,
abnormal session frequency, brand-new nodes, datacenter-ASN-like sustained
throughput), trains a classifier, evaluates it on a held-out split, and
saves the artifact for the serving API to load.

Honest framing: the *training data is synthetic* because there's no real
production traffic to learn from yet. The pipeline itself — feature
engineering, train/test split, model selection, evaluation, versioned
artifact — is the real, reusable part. Swapping in real historical
`bandwidth_sessions` + confirmed-fraud labels once the platform has volume
is a data change, not a code change.
"""
import numpy as np
import pandas as pd
from sklearn.ensemble import RandomForestClassifier
from sklearn.model_selection import train_test_split
from sklearn.metrics import classification_report, roc_auc_score
import joblib
import json
import os

RNG = np.random.default_rng(42)
N = 20000
MODEL_VERSION = "v1-synthetic-2026-09"

def generate_synthetic_dataset(n):
    # Latent "is this actually fraud" ground truth, built from a few
    # independent risk factors — each individually noisy, combined they
    # produce a realistic-shaped classification problem (not linearly
    # separable on any single feature, which is the point: a model earns
    # its keep here precisely because no single rule catches everything).
    node_trust_score = np.clip(RNG.normal(55, 20, n), 0, 100)
    node_age_hours = np.clip(RNG.exponential(400, n), 0.1, None)
    sessions_last_hour = RNG.poisson(2, n)
    gateway_bytes = np.clip(RNG.lognormal(mean=15, sigma=1.5, size=n), 1000, None)
    is_datacenter_asn_signal = RNG.binomial(1, 0.08, n)  # ASN-lookup flag, noisy in practice

    # Byte mismatch: mostly near zero (framing overhead) for honest clients,
    # heavy-tailed for tampered ones.
    honest_mismatch = np.abs(RNG.normal(0, 0.01, n))
    fraud_mismatch = np.abs(RNG.normal(0.35, 0.25, n))
    is_latent_fraud = (
        (node_trust_score < 30).astype(int) * 0.35
        + (node_age_hours < 24).astype(int) * 0.2
        + (sessions_last_hour > 8).astype(int) * 0.25
        + is_datacenter_asn_signal * 0.4
        + RNG.normal(0, 0.15, n)
    ) > 0.45
    byte_mismatch_ratio = np.where(is_latent_fraud, fraud_mismatch, honest_mismatch)
    byte_mismatch_ratio = np.clip(byte_mismatch_ratio, 0, 1)

    df = pd.DataFrame({
        "byte_mismatch_ratio": byte_mismatch_ratio,
        "node_trust_score": node_trust_score,
        "node_age_hours_log": np.log1p(node_age_hours),
        "sessions_last_hour": sessions_last_hour,
        "gateway_bytes_log": np.log1p(gateway_bytes),
        "is_datacenter_asn_signal": is_datacenter_asn_signal,
        "label_fraud": is_latent_fraud.astype(int),
    })
    return df

def main():
    df = generate_synthetic_dataset(N)
    feature_cols = [c for c in df.columns if c != "label_fraud"]
    X, y = df[feature_cols], df["label_fraud"]

    X_train, X_test, y_train, y_test = train_test_split(X, y, test_size=0.2, random_state=42, stratify=y)

    model = RandomForestClassifier(
        n_estimators=200, max_depth=8, min_samples_leaf=20,
        class_weight="balanced", random_state=42, n_jobs=-1,
    )
    model.fit(X_train, y_train)

    y_pred = model.predict(X_test)
    y_proba = model.predict_proba(X_test)[:, 1]

    print("=== Evaluation on held-out test set ===")
    print(classification_report(y_test, y_pred, target_names=["legit", "fraud"]))
    auc = roc_auc_score(y_test, y_proba)
    print(f"ROC-AUC: {auc:.4f}")

    importances = pd.Series(model.feature_importances_, index=feature_cols).sort_values(ascending=False)
    print("\n=== Feature importances ===")
    print(importances.to_string())

    out_dir = os.path.dirname(os.path.abspath(__file__))
    joblib.dump(model, os.path.join(out_dir, "fraud_model.joblib"))
    with open(os.path.join(out_dir, "model_metadata.json"), "w") as f:
        json.dump({
            "version": MODEL_VERSION,
            "feature_order": feature_cols,
            "test_roc_auc": round(float(auc), 4),
            "trained_on": "synthetic",
            "n_train": len(X_train),
            "n_test": len(X_test),
        }, f, indent=2)

    print(f"\nSaved fraud_model.joblib and model_metadata.json ({MODEL_VERSION})")

if __name__ == "__main__":
    main()
