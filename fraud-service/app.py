"""
Fraud-scoring service. Loads the trained model and scores one session at a
time via HTTP. Called synchronously by the control plane's FinalizeSession —
if this service is unreachable or slow, the control plane still finalizes
the session (money movement must never block on a fraud model being up),
it just records the session as unscored.
"""
from flask import Flask, request, jsonify
import joblib
import json
import os
import time

BASE = os.path.dirname(os.path.abspath(__file__))
MODEL_PATH = os.path.join(BASE, "fraud_model.joblib")
META_PATH = os.path.join(BASE, "model_metadata.json")

app = Flask(__name__)

_model = None
_meta = None

def load_model():
    global _model, _meta
    _model = joblib.load(MODEL_PATH)
    with open(META_PATH) as f:
        _meta = json.load(f)
    print(f"Loaded fraud model {_meta['version']} (test ROC-AUC {_meta['test_roc_auc']})")

load_model()

FLAG_THRESHOLD = 0.5  # score above this trips ml_fraud_flag; tune against real outcomes later

@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok", "model_version": _meta["version"]})

@app.route("/score", methods=["POST"])
def score():
    body = request.get_json(force=True, silent=True) or {}

    gateway_bytes = float(body.get("gateway_confirmed_bytes", 0))
    client_bytes = float(body.get("client_reported_bytes", 0))
    byte_mismatch_ratio = abs(gateway_bytes - client_bytes) / max(gateway_bytes, 1.0)
    byte_mismatch_ratio = min(byte_mismatch_ratio, 1.0)

    import math
    features = {
        "byte_mismatch_ratio": byte_mismatch_ratio,
        "node_trust_score": float(body.get("node_trust_score", 50.0)),
        "node_age_hours_log": math.log1p(float(body.get("node_age_hours", 1.0))),
        "sessions_last_hour": int(body.get("sessions_last_hour", 0)),
        "gateway_bytes_log": math.log1p(gateway_bytes),
        "is_datacenter_asn_signal": int(bool(body.get("is_datacenter_asn", False))),
    }

    X = [[features[col] for col in _meta["feature_order"]]]
    proba = float(_model.predict_proba(X)[0][1])
    flagged = proba >= FLAG_THRESHOLD

    return jsonify({
        "score": round(proba, 5),
        "flagged": flagged,
        "model_version": _meta["version"],
        "features": features,
    })

if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8082))
    app.run(host="0.0.0.0", port=port)
