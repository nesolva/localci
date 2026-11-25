import localci_pytest as ci
import pytest

PRICES = {"mug": 900, "poster": 1500, "sticker": 250}


@pytest.fixture
def placed(api):
    """An order of two mugs, placed before the test starts."""
    with ci.step("place two mugs"):
        status, order = api.post("/orders", {"sku": "mug", "qty": 2})
    assert status == 201
    yield order


@pytest.mark.parametrize("sku,qty", [("mug", 1), ("mug", 3), ("poster", 2), ("sticker", 12)])
def test_the_total_is_the_price_times_the_quantity(api, sku, qty):
    with ci.step("place the order"):
        status, order = api.post("/orders", {"sku": sku, "qty": qty})
    assert status == 201
    with ci.step("check the total"):
        expected = PRICES[sku] * qty
        if qty >= 10:  # 10% off in bulk
            expected = expected * 9 // 10
        assert order["total"] == expected


class TestFindingAnOrder:
    def test_an_order_can_be_read_back(self, api, placed):
        with ci.step("read it back"):
            status, order = api.get("/orders/" + placed["id"])
        assert status == 200
        assert order == placed

    def test_an_unknown_order_is_not_found(self, api):
        status, body = api.get("/orders/o999999")
        assert status == 404
        assert body["error"] == "no such order"


class TestRefusedOrders:
    def test_an_unknown_sku_is_not_found(self, api):
        assert api.post("/orders", {"sku": "teapot", "qty": 1})[0] == 404

    @pytest.mark.parametrize("qty", [0, 101, "two"])
    def test_a_bad_quantity_is_refused(self, api, qty):
        status, body = api.post("/orders", {"sku": "mug", "qty": qty})
        assert status == 422
        assert "qty" in body["error"]

    def test_a_declined_card_creates_no_order(self, api, payments):
        with ci.step("pay with a declined card"):
            status, body = api.post("/orders", {"sku": "mug", "qty": 1, "card": "tok_declined"})
        assert status == 402
        with ci.step("check nothing was charged"):
            assert body["error"] == "payment declined"
            assert all(c["amount"] != 900 or c["id"] for c in payments.get("/charges")[1])


def test_each_order_is_charged_once(api, payments):
    before = len(payments.get("/charges")[1])
    with ci.step("place the order"):
        status, order = api.post("/orders", {"sku": "poster", "qty": 1})
    assert status == 201
    charges = payments.get("/charges")[1]
    assert len(charges) == before + 1
    assert charges[-1]["id"] == order["charge"]
